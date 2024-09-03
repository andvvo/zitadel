package setup

import (
	"context"
	"embed"

	"github.com/zitadel/zitadel/internal/database"
	"github.com/zitadel/zitadel/internal/eventstore"
)

var (
	//go:embed 32/cockroach/32.sql
	//go:embed 32/postgres/32.sql
	eventPositionDefault embed.FS
)

type EventPositionDefault struct {
	dbClient *database.DB
}

func (mig *EventPositionDefault) Execute(ctx context.Context, _ eventstore.Event) error {
	stmt, err := readStmt(eventPositionDefault, "32", mig.dbClient.Type(), "32.sql")
	if err != nil {
		return err
	}
	_, err = mig.dbClient.ExecContext(ctx, stmt)
	return err
}

func (mig *EventPositionDefault) String() string {
	return "32_event_position_default"
}
