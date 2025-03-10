package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bufbuild/connect-go"
	"github.com/bufbuild/protovalidate-go"
	"github.com/sirupsen/logrus"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1/calendarv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/auth"
	"github.com/tierklinik-dobersberg/apis/pkg/cors"
	"github.com/tierklinik-dobersberg/apis/pkg/discovery"
	"github.com/tierklinik-dobersberg/apis/pkg/discovery/consuldiscover"
	"github.com/tierklinik-dobersberg/apis/pkg/discovery/wellknown"
	"github.com/tierklinik-dobersberg/apis/pkg/log"
	"github.com/tierklinik-dobersberg/apis/pkg/privacy"
	"github.com/tierklinik-dobersberg/apis/pkg/server"
	"github.com/tierklinik-dobersberg/apis/pkg/validator"
	"github.com/tierklinik-dobersberg/cis-cal/internal/app"
	"github.com/tierklinik-dobersberg/cis-cal/internal/config"
	"github.com/tierklinik-dobersberg/cis-cal/internal/services"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func main() {
	ctx := context.Background()

	configPath := os.Getenv("CONFIG_FILE")

	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	if configPath == "" {
		workdir, err := os.Getwd()
		if err != nil {
			logrus.Fatalf("failed to get working directory: %s", err.Error())
		}

		configPath = filepath.Join(workdir, "config.yml")
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		logrus.Fatalf("failed to load configuration: %s", err)
	}

	app, err := app.New(ctx, cfg)
	if err != nil {
		logrus.Fatalf("failed to prepare application providers: %s", err)
	}

	protoValidator, err := protovalidate.New()
	if err != nil {
		logrus.Fatalf("failed to prepare proto validator: %s", err)
	}

	authInterceptor := auth.NewAuthAnnotationInterceptor(
		protoregistry.GlobalFiles,
		auth.NewIDMRoleResolver(app.Roles),
		auth.RemoteHeaderExtractor,
	)

	logInterceptor := log.NewLoggingInterceptor()
	validatorInterceptor := validator.NewInterceptor(protoValidator)
	privacyInterceptor := privacy.NewFilterInterceptor(
		privacy.SubjectResolverFunc(func(ctx context.Context, ar connect.AnyRequest) (string, []string, error) {
			userId := ar.Header().Get("X-Remote-User-ID")
			roles := ar.Header().Values("X-Remote-Role")

			return userId, roles, nil
		}),
	)

	interceptors := connect.WithInterceptors(
		logInterceptor,
		authInterceptor,
		validatorInterceptor,
		privacyInterceptor,
	)

	serveMux := http.NewServeMux()

	calService := services.New(ctx, app)
	path, handler := calendarv1connect.NewCalendarServiceHandler(calService, interceptors)
	serveMux.Handle(path, handler)

	holidayService := services.NewHolidayService(cfg.DefaultCountry)
	path, handler = calendarv1connect.NewHolidayServiceHandler(holidayService, interceptors)
	serveMux.Handle(path, handler)

	corsOpts := cors.Config{
		AllowedOrigins:   cfg.AllowedOrigins,
		AllowCredentials: true, // we need allow-credentials here as browsers need to send the token for the forward-auth endpoint
		Debug:            os.Getenv("DEBUG_CORS") != "",
	}

	// Register at service catalog
	catalog, err := consuldiscover.NewFromEnv()
	if err != nil {
		logrus.Fatalf("failed to get service catalog client: %s", err)
	}

	if err := discovery.Register(ctx, catalog, &discovery.ServiceInstance{
		Name:    wellknown.CalendarV1ServiceScope,
		Address: cfg.ListenAddress,
	}); err != nil {
		logrus.Errorf("failed to register service at catalog: %s", err)
	}

	httpServer := server.Create(
		cfg.ListenAddress,
		cors.Wrap(corsOpts, serveMux),
	)

	if err := server.Serve(ctx, httpServer); err != nil {
		logrus.Fatalf("failed to listen and serve: %s", err)
	}
}
