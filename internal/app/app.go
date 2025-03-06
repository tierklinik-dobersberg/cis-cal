package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1/eventsv1connect"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/idm/v1/idmv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
	"github.com/tierklinik-dobersberg/cis-cal/internal/config"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo/google"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo/ical"
	"github.com/tierklinik-dobersberg/cis-cal/internal/resources"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type App struct {
	Config    config.Config
	Users     idmv1connect.UserServiceClient
	Roles     idmv1connect.RoleServiceClient
	Events    eventsv1connect.EventServiceClient
	Resources *resources.Database
	ICalRepo  *ical.Repository

	Google repo.ReadWriter
}

func New(ctx context.Context, cfg config.Config) (*App, error) {
	// Connect and ping MongoDB
	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.MongoURL))
	if err != nil {
		return nil, err
	}
	if err := mongoClient.Ping(ctx, nil); err != nil {
		return nil, err
	}

	// prepare the ical calendars
	icalRepo := ical.New()
	for _, cfg := range cfg.ICals {
		if err := icalRepo.Add(cfg); err != nil {
			return nil, fmt.Errorf("failed to add ical calendar to repository: %w", err)
		}
	}

	// prepare the resource database
	resourceDb, err := resources.NewDatabase(ctx, mongoClient.Database(cfg.MongoDatabaseName))
	if err != nil {
		return nil, err
	}

	// prepare the google calendar repository
	service, err := google.New(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare google calendar backend: %w", err)
	}

	app := &App{
		Google: service,

		Config:    cfg,
		Users:     idmv1connect.NewUserServiceClient(http.DefaultClient, cfg.IdmURL),
		Roles:     idmv1connect.NewRoleServiceClient(http.DefaultClient, cfg.IdmURL),
		Events:    eventsv1connect.NewEventServiceClient(cli.NewInsecureHttp2Client(), cfg.EventsServiceUrl),
		Resources: resourceDb,
		ICalRepo:  icalRepo,
	}

	return app, nil
}
