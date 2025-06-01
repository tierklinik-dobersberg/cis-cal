package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1/eventsv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
	"github.com/tierklinik-dobersberg/cis-cal/internal/config"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
	"github.com/tierklinik-dobersberg/cis/pkg/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/singleflight"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/encoding/protojson"
)

type GoogleBackend struct {
	*calendar.Service

	EventsClient    eventsv1connect.EventServiceClient
	ignoreCalendars []string

	cacheLock   sync.Mutex
	eventsCache map[string]*googleEventCache
	loadGroup   singleflight.Group
}

// New creates a new calendar service from cfg.
func New(ctx context.Context, cfg config.Config) (*GoogleBackend, error) {
	creds, err := credsFromFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read credentials file %s: %w", cfg.CredentialsFile, err)
	}

	token, err := tokenFromFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read token from %s: %w", cfg.TokenFile, err)
	}

	client := creds.Client(ctx, token)
	calSvc, err := calendar.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("failed to create calendar client: %w", err)
	}

	svc := &GoogleBackend{
		Service:         calSvc,
		eventsCache:     make(map[string]*googleEventCache),
		ignoreCalendars: cfg.IgnoreCalendars,
		EventsClient:    eventsv1connect.NewEventServiceClient(cli.NewInsecureHttp2Client(), cfg.EventsServiceUrl),
	}

	// create a new eventCache for each calendar right now
	if _, err := svc.ListCalendars(ctx); err != nil {
		slog.Error("failed to start watching calendars", "erro", err)
	}

	return svc, nil
}

// Authenticate retrieves a new token and saves it under TokenFile.
func Authenticate(cfg config.Config) error {
	creds, err := credsFromFile(cfg.CredentialsFile)
	if err != nil {
		return fmt.Errorf("failed reading %s: %w", cfg.CredentialsFile, err)
	}

	token, err := getTokenFromWeb(creds)
	if err != nil {
		return err
	}

	if err := saveTokenFile(token, cfg.TokenFile); err != nil {
		return err
	}

	return nil
}

func (svc *GoogleBackend) ListCalendars(ctx context.Context) ([]repo.Calendar, error) {
	res, err := svc.Service.CalendarList.List().ShowHidden(true).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve list of calendars: %w", err)
	}

	var list = make([]repo.Calendar, 0, len(res.Items))
	for _, item := range res.Items {
		// check if the calendar should be ingored based on IngoreCalendar=
		if svc.shouldIngore(item) {
			continue
		}

		list = append(list, repo.Calendar{
			ID:       item.Id,
			Name:     item.Summary,
			Timezone: item.TimeZone,
			Color:    item.BackgroundColor,
			Reader:   svc,
		})

		// immediately prepare the calendar cache
		if _, err = svc.cacheFor(ctx, item.Id); err != nil {
			logrus.Errorf("failed to perpare calendar event cache for %s: %s", item.Id, err)
		}
	}

	return list, nil
}

func (svc *GoogleBackend) ListEvents(ctx context.Context, calendarID string, searchOpts ...repo.SearchOption) ([]repo.Event, error) {
	opts := new(repo.EventSearchOptions)

	for _, fn := range searchOpts {
		fn(opts)
	}

	cache, err := svc.cacheFor(ctx, calendarID)
	if err != nil {
		logrus.Errorf("failed to get event cache for calendar %s: %s", calendarID, err)
	}

	events, ok := cache.tryLoadFromCache(ctx, opts)
	if ok {
		return events, nil
	}

	return svc.loadEvents(ctx, calendarID, opts, cache)
}

func getExtendedProps(resources []string, data *calendarv1.CustomerAnnotation) map[string]string {
	props := make(map[string]string, 2)
	if data != nil {
		if data.CustomerId != "" {
			props["tkd.calendar.v1.customerId"] = data.CustomerId
		}

		jsonBlob, err := protojson.Marshal(data)
		if err != nil {
			slog.Error("failed to marshal customer annoations", "error", err)
		} else {
			props["tkd.calendar.v1.CustomerAnnotation"] = string(jsonBlob)
		}
	}

	if len(resources) > 0 {
		jsonBlob, err := json.Marshal(resources)
		if err != nil {
			slog.Error("failed to marshal resource-name annoations", "error", err)
		} else {
			props["tkd.calendar.v1.ResourceNames"] = string(jsonBlob)
		}
	}

	return props
}

func (svc *GoogleBackend) CreateEvent(ctx context.Context, calID, name, description string, startTime time.Time, duration time.Duration, resources []string, data *calendarv1.CustomerAnnotation) (*repo.Event, error) {
	ctx, sp := otel.Tracer("").Start(ctx, "google.backend#CreateEvent")
	defer sp.End()

	sp.SetAttributes(
		attribute.String("calendar.id", calID),
		attribute.String("calendar.name", name),
		attribute.String("calendar.description", description),
		attribute.String("calendar.start_time", startTime.String()),
		attribute.String("calendar.duration", duration.String()),
	)

	res, err := svc.Service.Events.Insert(calID, &calendar.Event{
		Summary:     name,
		Description: description,
		Start: &calendar.EventDateTime{
			DateTime: startTime.Format(time.RFC3339),
		},
		End: &calendar.EventDateTime{
			DateTime: startTime.Add(duration).Format(time.RFC3339),
		},
		Status: "confirmed",
		ExtendedProperties: &calendar.EventExtendedProperties{
			Shared: getExtendedProps(resources, data),
		},
	}).Context(ctx).Do()
	if err != nil {
		trace.RecordAndLog(ctx, err)

		return nil, fmt.Errorf("failed to insert event upstream: %w", err)
	}
	logrus.Infof("created event with id %s", res.Id)

	if cache, _ := svc.cacheFor(ctx, calID); cache != nil {
		cache.triggerSync()
	}

	return googleEventToModel(ctx, calID, res)
}

func (svc *GoogleBackend) UpdateEvent(ctx context.Context, event repo.Event) (*repo.Event, error) {
	evt, err := svc.Service.Events.Update(event.CalendarID, event.ID, &calendar.Event{
		Summary:     event.Summary,
		Description: event.Description,
		Start: &calendar.EventDateTime{
			DateTime: event.StartTime.Format(time.RFC3339),
		},
		End: &calendar.EventDateTime{
			DateTime: event.EndTime.Format(time.RFC3339),
		},
		Status: "confirmed",
		ExtendedProperties: &calendar.EventExtendedProperties{
			Shared: getExtendedProps(event.Resources, event.CustomerAnnotation),
		},
	}).Context(ctx).Do()

	if err != nil {
		return nil, err
	}

	if cache, err := svc.cacheFor(ctx, event.CalendarID); err == nil && cache != nil {
		cache.triggerSync()
	} else {
		logrus.Errorf("[update] failed to trigger sync for event calendar id %q: %s", event.CalendarID, err)
	}

	return googleEventToModel(ctx, event.CalendarID, evt)
}

func (svc *GoogleBackend) MoveEvent(ctx context.Context, originCalendarId string, eventId string, targetCalendarId string) (*repo.Event, error) {
	result, err := svc.Service.Events.Move(originCalendarId, eventId, targetCalendarId).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	if cache, err := svc.cacheFor(ctx, originCalendarId); err == nil && cache != nil {
		// delete the event from the cache and trigger a sync
		cache.deleteEvent(eventId)
		cache.triggerSync()
	} else {
		logrus.Errorf("[move] failed to trigger sync for origin calendar id %q: %s", originCalendarId, err)
	}

	if cache, err := svc.cacheFor(ctx, targetCalendarId); err == nil && cache != nil {
		cache.triggerSync()
	} else {
		logrus.Errorf("[move] failed to trigger sync for target calendar id %q: %s", targetCalendarId, err)
	}

	return googleEventToModel(ctx, targetCalendarId, result)
}

func (svc *GoogleBackend) DeleteEvent(ctx context.Context, calID, eventID string) error {
	err := svc.Service.Events.Delete(calID, eventID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to delete event upstream: %w", err)
	}

	cache, err := svc.cacheFor(ctx, calID)
	if err == nil {
		cache.deleteEvent(eventID)
		cache.triggerSync()
	}

	return nil
}

func (svc *GoogleBackend) cacheFor(ctx context.Context, calID string) (*googleEventCache, error) {
	svc.cacheLock.Lock()
	defer svc.cacheLock.Unlock()

	cache, ok := svc.eventsCache[calID]
	if ok {
		logrus.Debugf("using existing event cache for %s", calID)

		return cache, nil
	}

	cache, err := newCache(ctx, calID, calID, svc.Service, svc.EventsClient)
	if err != nil {
		return nil, err
	}

	svc.eventsCache[calID] = cache
	logrus.Debugf("created new event cache for calendar %s", calID)

	return cache, nil
}

func (svc *GoogleBackend) LoadEvent(ctx context.Context, calendarID, eventID string, ignoreCache bool) (*repo.Event, error) {
	opts := &repo.EventSearchOptions{
		EventID: &eventID,
	}
	if !ignoreCache {
		if cache, err := svc.cacheFor(ctx, calendarID); err == nil && cache != nil {
			events, ok := cache.tryLoadFromCache(ctx, opts)
			if ok && len(events) == 1 {
				return &events[0], nil
			}
		}
	}

	evt, err := svc.Service.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		var googleError *googleapi.Error
		if errors.As(err, &googleError) {
			switch googleError.Code {
			case http.StatusNotFound, http.StatusGone:
				return nil, repo.ErrNotFound
			}
		}

		return nil, err
	}

	return googleEventToModel(ctx, calendarID, evt)
}

// trunk-ignore(golangci-lint/cyclop)
func (svc *GoogleBackend) loadEvents(ctx context.Context, calendarID string, searchOpts *repo.EventSearchOptions, cache *googleEventCache) ([]repo.Event, error) {
	call := svc.Events.List(calendarID).ShowDeleted(false).SingleEvents(true)

	key := calendarID
	if searchOpts != nil {
		if searchOpts.FromTime != nil {
			call = call.TimeMin(searchOpts.FromTime.Format(time.RFC3339))
			key += fmt.Sprintf("-%s", searchOpts.FromTime.Format(time.RFC3339))
		}

		upper := cache.currentMinTime()

		if searchOpts.ToTime != nil && searchOpts.ToTime.After(upper) {
			upper = *searchOpts.ToTime
		}

		call = call.TimeMax(upper.Format(time.RFC3339))
		key += fmt.Sprintf("-%s", upper.Format(time.RFC3339))

		if searchOpts.EventID != nil {
			key += "-" + *searchOpts.EventID
		}

		if searchOpts.CustomerID != nil {
			key += "-" + *searchOpts.CustomerID
			call = call.SharedExtendedProperty(fmt.Sprintf("tkd.calendar.v1.customerId=%s", *searchOpts.CustomerID))
		}

		if searchOpts.SearchText != nil {
			key += "-" + *searchOpts.SearchText
			call = call.Q(*searchOpts.SearchText)
		}
	}

	res, err, _ := svc.loadGroup.Do(key, func() (interface{}, error) {
		var events []repo.Event
		var pageToken string
		for {
			if pageToken != "" {
				call.PageToken(pageToken)
			}
			res, err := call.Context(ctx).Do()
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve page from upstream: %w", err)
			}

			for _, item := range res.Items {
				evt, err := googleEventToModel(ctx, calendarID, item)

				if err != nil {
					logrus.Error(err.Error())

					continue
				}

				// if we're searching for a single event ID, we can check for that ID and
				// exit early
				if searchOpts.EventID != nil {
					if evt.ID == *searchOpts.EventID {
						return []repo.Event{*evt}, nil
					}
				} else {
					events = append(events, *evt)
				}

			}

			if res.NextPageToken != "" {
				pageToken = res.NextPageToken

				continue
			}

			break
		}

		return events, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to fetch events: %w", err)
	}

	return res.([]repo.Event), nil
}

func (svc *GoogleBackend) shouldIngore(item *calendar.CalendarListEntry) bool {
	return slices.Contains(svc.ignoreCalendars, item.Id)
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var token oauth2.Token
	if err := json.Unmarshal(content, &token); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON token: %w", err)
	}

	return &token, nil
}

func saveTokenFile(token *oauth2.Token, path string) error {
	blob, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON token: %w", err)
	}

	return os.WriteFile(path, blob, 0600)
}

func credsFromFile(path string) (*oauth2.Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	config, err := google.ConfigFromJSON(content, calendar.CalendarScope, "https://www.googleapis.com/auth/userinfo.profile")
	if err != nil {
		return nil, fmt.Errorf("failed to get configuration from JSON: %w", err)
	}

	return config, nil
}

func getTokenFromWeb(config *oauth2.Config) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+ //nolint:forbidigo
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		return nil, fmt.Errorf("unable to read authorization code: %w", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve token: %w", err)
	}

	return tok, nil
}
