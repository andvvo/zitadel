package mirror

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/zitadel/logging"

	db "github.com/zitadel/zitadel/internal/database"
	"github.com/zitadel/zitadel/internal/database/dialect"
	"github.com/zitadel/zitadel/internal/id"
	"github.com/zitadel/zitadel/internal/v2/database"
	"github.com/zitadel/zitadel/internal/v2/eventstore"
	"github.com/zitadel/zitadel/internal/v2/eventstore/postgres"
	"github.com/zitadel/zitadel/internal/v2/readmodel"
	"github.com/zitadel/zitadel/internal/zerrors"
)

var shouldIgnorePrevious bool

func eventstoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eventstore",
		Short: "mirrors the eventstore of an instance between databases, or between a database and files",
		Long: `mirrors the eventstore of an instance between databases, or between a database and files
ZITADEL needs to be initialized and set up with the --for-mirror flag
Migrate only copies events2 and unique constraints`,
		Run: func(cmd *cobra.Command, args []string) {
			config := mustNewMigrationConfig(viper.GetViper())
			copyEventstore(cmd.Context(), config)
		},
	}

	cmd.Flags().BoolVar(&shouldReplace, "replace", false, "allow delete unique constraints of defined instances before copy")
	cmd.Flags().BoolVar(&shouldIgnorePrevious, "ignore-previous", false, "ignores previous migrations of the events table")

	return cmd
}

func copyEventstore(ctx context.Context, config *Migration) {
	ctx, cancel := context.WithCancel(ctx)
	errs := make(chan error, 2)
	readers := make(chan *io.PipeReader)
	previousMigration := initDestination(ctx, config, readers, errs)
	initSource(ctx, config, previousMigration, readers, errs)
	// await the state of source and destination
	for i := 0; i < 2; i++ {
		err := <-errs
		if err != nil {
			logging.WithError(err).Warn("error during migration")
			cancel()
		}
	}
	close(errs)
	cancel()
}

func initDestination(ctx context.Context, config *Migration, readers <-chan *io.PipeReader, errs chan<- error) (previousMigration *readmodel.LastSuccessfulMirror) {
	switch {
	case config.Destination.Database != nil:
		return initDBDestination(ctx, config, readers, errs)
	case config.Destination.File != nil:
		initFileDestination(ctx, config.Destination.File, readers, errs)
		return nil
	}
	logging.Fatal("destination not defined it must be either a database or a file")
	return nil
}

func initFileDestination(ctx context.Context, config *fileLocation, readers <-chan *io.PipeReader, errs chan<- error) {
	eventsFolderPath := filepath.Join(config.Path, "eventstore", "events2")

	err := os.MkdirAll(eventsFolderPath, 0777)
	logging.OnError(err).WithField("path", eventsFolderPath).Fatal("unable to create destination folder")

	go func() {
		for i := 1; ; i++ {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case reader := <-readers:
				if reader == nil {
					errs <- nil
					return
				}
				if err = writeToFile(filepath.Join(eventsFolderPath, fmt.Sprintf("%010d.csv", i)), reader); err != nil {
					errs <- err
					return
				}
			}
		}
	}()
}

func writeToFile(path string, reader *io.PipeReader) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return reader.CloseWithError(err)
	}
	defer func() {
		closeErr := reader.CloseWithError(file.Close())
		if err == nil {
			err = closeErr
		}
	}()
	_, err = io.Copy(file, reader)
	return err
}

func initDBDestination(ctx context.Context, config *Migration, readers <-chan *io.PipeReader, errs chan<- error) *readmodel.LastSuccessfulMirror {
	client, err := db.Connect(*config.Destination.Database, false, dialect.DBPurposeEventPusher)
	logging.OnError(err).Fatal("unable to connect to destination database")

	conn, err := client.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire destination connection")

	eventStore := eventstore.NewEventstoreFromOne(postgres.New(client, &postgres.Config{
		MaxRetries: 3,
	}))

	previousMigration, err := queryLastSuccessfulMigration(ctx, eventStore, config.SourceName())
	logging.OnError(err).Fatal("unable to query latest successful migration")

	go func() {
		defer client.Close()
		defer conn.Close()
		for {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case reader := <-readers:
				if reader == nil {
					errs <- nil
					return
				}

				err = conn.Raw(func(driverConn interface{}) error {
					conn := driverConn.(*stdlib.Conn).Conn()
					_, err := conn.PgConn().CopyFrom(ctx, reader, "COPY eventstore.events2 (instance_id, aggregate_type, "+
						"aggregate_id, event_type, sequence, revision, "+
						"created_at, payload, creator, owner, in_tx_order) "+
						" FROM STDIN (FORMAT csv)")
					return reader.CloseWithError(err)
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}
	}()
	return previousMigration
}

func initSource(ctx context.Context, config *Migration, previousMigration *readmodel.LastSuccessfulMirror, readers chan<- *io.PipeReader, errs chan<- error) {
	switch {
	// case config.Source.Database != nil:
	// 	initDBSource(ctx, config, previousMigration, readers, errs)
	// 	return
	case config.Source.File != nil:
		initFileSource(ctx, config.Source.File, readers, errs)
		return
	}
	logging.Fatal("source not defined it must be either a database or a file")
}

func initFileSource(ctx context.Context, config *fileLocation, readers chan<- *io.PipeReader, errs chan<- error) {
	eventsFolderPath := filepath.Join(config.Path, "eventstore", "events2")

	folder, err := os.Open(eventsFolderPath)
	logging.OnError(err).WithField("path", eventsFolderPath).Fatal("unable to open source folder")
	defer folder.Close()

	files, err := folder.ReadDir(-1)
	logging.OnError(err).Fatal("unable to read files in source folder")

	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	go func() {
		for _, file := range files {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			default:
				if file.IsDir() {
					continue
				}
				err = readFromFile(eventsFolderPath, file, readers)
				if err != nil {
					errs <- err
					return
				}
			}
		}
		defer close(readers)
		errs <- nil
	}()
}

func readFromFile(path string, file fs.DirEntry, readers chan<- *io.PipeReader) (err error) {
	srcFile, err := os.OpenFile(filepath.Join(path, file.Name()), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	reader, writer := io.Pipe()
	readers <- reader
	defer writer.Close()

	_, err = srcFile.WriteTo(writer)
	return err
}

func initDBSource(ctx context.Context, config *Migration, previousMigration *readmodel.LastSuccessfulMirror, readers chan<- *io.PipeReader, errs chan<- error) {
	migrationID, err := id.SonyFlakeGenerator().Next()
	logging.OnError(err).Fatal("unable to generate migration id")

	client, err := db.Connect(*config.Source.Database, false, dialect.DBPurposeEventPusher)
	logging.OnError(err).Fatal("unable to connect to source database")

	eventStore := eventstore.NewEventstoreFromOne(postgres.New(client, &postgres.Config{
		MaxRetries: 3,
	}))

	maxPosition, err := writeMigrationStart(ctx, eventStore, migrationID, config.DestinationName())
	logging.OnError(err).Fatal("unable to write migration started event")

	conn, err := client.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire source connection")

	var from float64
	if previousMigration != nil && !shouldIgnorePrevious {
		from = previousMigration.Position
	}

	logging.WithFields("from", from, "to", maxPosition).Info("start event migration")

	go func() {
		var eventCount int64

		defer client.Close()
		defer conn.Close()
		defer close(readers)

		err = conn.Raw(func(driverConn interface{}) error {
			conn := driverConn.(*stdlib.Conn).Conn()
			for i := uint32(0); ; i++ {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					bulkCount, err := readFromDB(ctx, conn, config.EventBulkSize, from, maxPosition, i, readers)
					if err != nil {
						return err
					}
					eventCount += bulkCount
					if bulkCount < int64(config.EventBulkSize) {
						return nil
					}
				}
			}
		})

		errs <- err
		if err != nil {
			return
		}
		logging.WithFields("count", eventCount).Info("events migrated")
	}()
}

func readFromDB(ctx context.Context, conn *pgx.Conn, bulkSize uint32, from, maxPosition float64, idx uint32, readers chan<- *io.PipeReader) (eventCount int64, err error) {
	var stmt database.Statement
	stmt.WriteString("COPY (SELECT *, row_number() OVER () FROM (SELECT instance_id, aggregate_type, " +
		"aggregate_id, event_type, sequence, revision, " +
		"created_at, regexp_replace(payload::TEXT, '\\\\u0000', '', 'g')::JSON " +
		"payload, creator, owner " +
		"AS in_tx_order FROM eventstore.events2 ")
	stmt.WriteString(instanceClause())
	stmt.WriteString(" AND ")
	database.NewNumberAtMost(maxPosition).Write(&stmt, "position")
	stmt.WriteString(" AND ")
	database.NewNumberGreater(from).Write(&stmt, "position")
	stmt.WriteString(" ORDER BY instance_id, position, in_tx_order")
	stmt.WriteString(" LIMIT ")
	stmt.WriteArg(bulkSize)
	stmt.WriteString(" OFFSET ")
	stmt.WriteArg(bulkSize * idx)
	stmt.WriteString(")) TO STDOUT (FORMAT csv)")

	reader, writer := io.Pipe()
	readers <- reader

	defer writer.Close()

	// Copy does not allow args so we use we replace the args in the statement
	tag, err := conn.PgConn().CopyTo(ctx, writer, stmt.Debug())
	if err != nil {
		return 0, zerrors.ThrowUnknownf(err, "MIGRA-KTuSq", "unable to copy events from source during iteration %d", idx)
	}
	return tag.RowsAffected(), nil
}

func writeCopyEventsDone(ctx context.Context, es *eventstore.EventStore, id, source string, position float64, errs <-chan error) {
	joinedErrs := make([]error, 0, len(errs))
	for err := range errs {
		joinedErrs = append(joinedErrs, err)
	}
	err := errors.Join(joinedErrs...)

	if err != nil {
		logging.WithError(err).Error("unable to mirror events")
		err := writeMigrationFailed(ctx, es, id, source, err)
		logging.OnError(err).Fatal("unable to write failed event")
		return
	}

	err = writeMigrationSucceeded(ctx, es, id, source, position)
	logging.OnError(err).Fatal("unable to write failed event")
}

func copyUniqueConstraintsDB(ctx context.Context, source, dest *db.DB) {
	start := time.Now()

	sourceConn, err := source.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire source connection")
	defer sourceConn.Close()

	reader, writer := io.Pipe()
	errs := make(chan error, 1)

	go func() {
		err := sourceConn.Raw(func(driverConn interface{}) error {
			conn := driverConn.(*stdlib.Conn).Conn()

			var stmt database.Statement
			stmt.WriteString("COPY (SELECT instance_id, unique_type, unique_field FROM eventstore.unique_constraints ")
			stmt.WriteString(instanceClause())
			stmt.WriteString(") TO STDOUT")

			_, err := conn.PgConn().CopyTo(ctx, writer, stmt.String())
			writer.Close()
			return err
		})
		errs <- err
	}()

	destConn, err := dest.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire destination connection")
	defer destConn.Close()

	var eventCount int64
	err = destConn.Raw(func(driverConn interface{}) error {
		conn := driverConn.(*stdlib.Conn).Conn()

		var stmt database.Statement
		if shouldReplace {
			stmt.WriteString("DELETE FROM eventstore.unique_constraints ")
			stmt.WriteString(instanceClause())

			_, err := conn.Exec(ctx, stmt.String())
			if err != nil {
				return err
			}
		}

		stmt.Reset()
		stmt.WriteString("COPY eventstore.unique_constraints FROM STDIN")

		tag, err := conn.PgConn().CopyFrom(ctx, reader, stmt.String())
		eventCount = tag.RowsAffected()

		return err
	})
	logging.OnError(err).Fatal("unable to copy unique constraints to destination")
	logging.OnError(<-errs).Fatal("unable to copy unique constraints from source")
	logging.WithFields("took", time.Since(start), "count", eventCount).Info("unique constraints migrated")
}

func copyUniqueConstraintsFromFile(ctx context.Context, dest *db.DB, fileName string) {
	start := time.Now()

	srcFile, err := os.OpenFile(filePath+fileName, os.O_RDONLY, 0)
	logging.OnError(err).Fatal("unable to open source file")
	defer srcFile.Close()

	reader, writer := io.Pipe()
	errs := make(chan error, 1)

	go func() {
		_, err := srcFile.WriteTo(writer)
		writer.Close()
		errs <- err
	}()

	destConn, err := dest.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire destination connection")
	defer destConn.Close()

	var eventCount int64
	err = destConn.Raw(func(driverConn interface{}) error {
		conn := driverConn.(*stdlib.Conn).Conn()

		var stmt database.Statement
		stmt.WriteString("DELETE FROM eventstore.unique_constraints ")
		stmt.WriteString(instanceClause())

		_, err := conn.Exec(ctx, stmt.String())
		if err != nil {
			return err
		}

		stmt.Reset()
		stmt.WriteString(`COPY eventstore.unique_constraints 
						(instance_id, unique_type, unique_field) 
						FROM STDIN (DELIMITER ',')`)

		tag, err := conn.PgConn().CopyFrom(ctx, reader, stmt.String())
		eventCount = tag.RowsAffected()

		return err
	})
	logging.OnError(err).Fatal("unable to copy unique constraints to destination")
	logging.OnError(<-errs).Fatal("unable to copy unique constraints from source")
	logging.WithFields("took", time.Since(start), "count", eventCount).Info("unique constraints migrated from " + fileName)
}

func copyUniqueConstraintsToFile(ctx context.Context, source *db.DB, fileName string) {
	start := time.Now()

	sourceConn, err := source.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire source connection")
	defer sourceConn.Close()

	reader, writer := io.Pipe()
	errs := make(chan error, 1)

	var eventCount int64
	go func() {
		err = sourceConn.Raw(func(driverConn interface{}) error {
			conn := driverConn.(*stdlib.Conn).Conn()

			var stmt database.Statement
			stmt.WriteString(`COPY (SELECT instance_id, unique_type, unique_field 
							FROM eventstore.unique_constraints `)
			stmt.WriteString(instanceClause())
			stmt.WriteString(") TO STDOUT (DELIMITER ',')")

			tag, err := conn.PgConn().CopyTo(ctx, writer, stmt.String())
			eventCount = tag.RowsAffected()
			writer.Close()

			return err
		})
		errs <- err
	}()

	destFile, err := os.OpenFile(filePath+fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	logging.OnError(err).Fatal("unable to open destination file")
	defer destFile.Close()

	_, err = io.Copy(destFile, reader)
	logging.OnError(err).Fatal("unable to copy unique constraints to destination")
	logging.OnError(<-errs).Fatal("unable to copy unique constraints from source")
	logging.WithFields("took", time.Since(start), "count", eventCount).Info("unique constraints copied to " + fileName)
}
