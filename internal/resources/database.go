package resources

import (
	"context"
	"fmt"

	"github.com/bufbuild/connect-go"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/pkg/mongomigrate"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Database struct {
	col *mongo.Collection
	db  *mongo.Database
}

const collectionName = "resource-calendars"

func NewDatabase(ctx context.Context, db *mongo.Database) (*Database, error) {
	d := &Database{
		col: db.Collection(collectionName),
		db:  db,
	}

	if err := d.setup(ctx); err != nil {
		return nil, err
	}

	return d, nil
}

func (db *Database) setup(ctx context.Context) error {
	m := mongomigrate.NewMigrator(db.db, "")

	m.Register(
		mongomigrate.Migration{
			Version:     1,
			Description: "Create indexes",
			Database:    db.db.Name(),
			Up: mongomigrate.MigrateFunc(
				func(sc mongo.SessionContext, d *mongo.Database) error {
					c := d.Collection(collectionName)

					_, err := c.Indexes().CreateOne(sc, mongo.IndexModel{
						Keys: bson.D{
							{
								Key:   "name",
								Value: 1,
							},
						},
						Options: options.Index().SetUnique(true),
					})

					return err
				},
			),
		},
	)

	return m.Run(ctx)
}

func (db *Database) Store(ctx context.Context, r *calendarv1.ResourceCalendar) error {
	_, err := db.col.InsertOne(ctx, ResourceCalendar{
		Name:             r.Name,
		DisplayName:      r.DisplayName,
		Color:            r.Color,
		MaxConcurrentUse: r.MaxConcurrentUse,
	})

	return err
}

func (db *Database) List(ctx context.Context) ([]*calendarv1.ResourceCalendar, error) {
	res, err := db.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}

	var results []ResourceCalendar
	if err := res.All(ctx, &results); err != nil {
		return nil, err
	}

	proto := make([]*calendarv1.ResourceCalendar, len(results))
	for idx, r := range results {
		proto[idx] = r.ToProto()
	}

	return proto, nil
}

func (db *Database) Delete(ctx context.Context, name string) error {
	res, err := db.col.DeleteOne(ctx, bson.M{"name": name})
	if err != nil {
		return err
	}

	if res.DeletedCount == 0 {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("resource-calendar %s not found", name))
	}

	return nil
}
