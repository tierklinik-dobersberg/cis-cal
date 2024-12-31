package repo

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	connect "github.com/bufbuild/connect-go"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	eventsv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1/eventsv1connect"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

type googleEventCache struct {
	rw            sync.RWMutex
	minTime       time.Time
	syncToken     string
	firstLoadDone chan struct{}
	trigger       chan struct{}

	calID        string
	calendarName string
	events       []Event
	svc          *calendar.Service
	eventService eventsv1connect.EventServiceClient
	wg           sync.WaitGroup

	log *slog.Logger
}

func (ec *googleEventCache) String() string {
	return fmt.Sprintf("Cache<%s>", ec.calID)
}

// nolint:unparam
func newCache(ctx context.Context, id string, name string, svc *calendar.Service, eventCli eventsv1connect.EventServiceClient) (*googleEventCache, error) {
	cache := &googleEventCache{
		calID:         id,
		calendarName:  name,
		svc:           svc,
		firstLoadDone: make(chan struct{}),
		trigger:       make(chan struct{}),
		eventService:  eventCli,
		log:           slog.With("calendar", name, "id", id),
	}

	cache.wg.Add(2)

	go cache.watch(ctx)
	go cache.evicter(ctx)

	<-cache.firstLoadDone

	return cache, nil
}

func (ec *googleEventCache) triggerSync() {
	select {
	case ec.trigger <- struct{}{}:
	default:
	}
}

func (ec *googleEventCache) watch(ctx context.Context) {
	defer ec.wg.Done()

	waitTime := time.Minute
	firstLoad := true
	for {
		success := ec.loadEvents(ctx)

		if success {
			waitTime = time.Minute
		} else {
			// in case of consecutive failures do some exponential backoff
			waitTime = 2 * waitTime
		}

		// cap at max 30 minute wait time
		if waitTime > time.Minute*30 {
			waitTime = time.Minute * 30
		}

		if firstLoad {
			firstLoad = false
			close(ec.firstLoadDone)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(waitTime):
		case <-ec.trigger:
		}
	}
}

func (ec *googleEventCache) loadEvents(ctx context.Context) bool {
	ec.rw.Lock()
	defer ec.rw.Unlock()

	call := ec.svc.Events.List(ec.calID)
	if ec.syncToken == "" {
		ec.events = nil
		now := time.Now().Local()
		currentMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		ec.minTime = currentMidnight

		call.ShowDeleted(false).SingleEvents(false).TimeMin(ec.minTime.Format(time.RFC3339))
	} else {
		call.SyncToken(ec.syncToken)
	}

	updatesProcessed := 0
	pageToken := ""
	for {
		if pageToken != "" {
			call.PageToken(pageToken)
		}

		res, err := call.Context(ctx).Do()
		if err != nil {
			if apiErr, ok := err.(*googleapi.Error); ok && apiErr.Code == http.StatusGone {
				// start over without a sync token
				// return "success" so we retry in a minute
				ec.syncToken = ""

				return true
			}

			ec.log.Error("failed to sync calendar events", "error", err)

			return false
		}

		for _, item := range res.Items {
			evt, change := ec.syncEvent(ctx, item)

			if evt == nil {
				continue
			}

			req := &calendarv1.CalendarChangeEvent{
				Calendar: ec.calID,
			}

			switch change {
			case "deleted":
				req.Kind = &calendarv1.CalendarChangeEvent_DeletedEventId{
					DeletedEventId: evt.ID,
				}
			default:
				p, err := evt.ToProto()
				if err != nil {
					ec.log.Error("failed to convert event to protobuf", "error", err)
				} else {
					req.Kind = &calendarv1.CalendarChangeEvent_EventChange{
						EventChange: p,
					}
				}
			}

			if req.Kind != nil {
				PublishEvent(ec.eventService, req, false)
			}
		}
		updatesProcessed += len(res.Items)

		if res.NextPageToken != "" {
			pageToken = res.NextPageToken

			continue
		}
		if res.NextSyncToken != "" {
			ec.syncToken = res.NextSyncToken

			break
		}

		// We should actually never reach this point as one of the above
		// if's should have matched. if we get her google apis returned
		// something unexpected so better clear anything we have and start
		// over.
		ec.log.Error("unexpected google api response, starting over")

		ec.syncToken = ""
		ec.events = nil
		ec.minTime = time.Time{}

		return false
	}
	if updatesProcessed > 0 {
		ec.log.Info("processed updates", "updates", updatesProcessed)
	}

	sort.Sort(ByStartTime(ec.events))

	return true
}

func (ec *googleEventCache) syncEvent(ctx context.Context, item *calendar.Event) (*Event, string) {
	foundAtIndex := -1
	for idx, evt := range ec.events {
		if evt.ID == item.Id {
			foundAtIndex = idx

			break
		}
	}
	if foundAtIndex > -1 {
		// check if the item has been deleted
		if item.Start == nil {
			evt := ec.events[foundAtIndex]
			ec.events = append(ec.events[:foundAtIndex], ec.events[foundAtIndex+1:]...)

			return &evt, "deleted"
		}

		// this should be an update
		evt, err := googleEventToModel(ctx, ec.calID, item)
		if err != nil {
			ec.log.Error("failed to convert event", "event-id", item.Id, "error", err)
			return nil, ""
		}
		ec.events[foundAtIndex] = *evt

		return evt, "updated"
	}

	evt, err := googleEventToModel(ctx, ec.calID, item)
	if err != nil {
		ec.log.Error("failed to convert event", "event-id", item.Id, "error", err)
		return nil, ""
	}
	ec.events = append(ec.events, *evt)

	return evt, "created"
}

func (ec *googleEventCache) evicter(ctx context.Context) {
	defer ec.wg.Done()

	ticker := time.NewTicker(time.Hour)

	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}

		ec.evictEvents()
	}
}

func (ec *googleEventCache) evictEvents() {
	now := time.Now().Local()
	currentMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	ec.rw.Lock()
	defer ec.rw.Unlock()

	filtered := make([]Event, 0, len(ec.events))
	countBefore := len(ec.events)

	for _, evt := range ec.events {
		if !currentMidnight.Before(evt.StartTime) {
			filtered = append(filtered, evt)
			continue
		}

		if evt.EndTime != nil && !evt.EndTime.Before(currentMidnight) {
			filtered = append(filtered, evt)
			continue
		}
	}

	ec.events = filtered
	ec.minTime = currentMidnight

	ec.log.Info("evicted events from cache", "evicted", countBefore-len(filtered), "cache-start-time", ec.minTime.Format(time.RFC3339), "cache-size", len(ec.events))
}

func (ec *googleEventCache) tryLoadFromCache(ctx context.Context, search *EventSearchOptions) ([]Event, bool) {
	// check if it's even possible to serve the request from cache.
	if search == nil {
		ec.log.Info("not using cache: search == nil")
		return nil, false
	}
	if search.FromTime == nil {
		ec.log.Info("not using cache: search.from == nil")
		return nil, false
	}

	ec.rw.RLock()
	defer ec.rw.RUnlock()

	if search.FromTime.Before(ec.minTime) && !ec.minTime.IsZero() {
		ec.log.Info("not using cache: search.from is before minTime", "search-time", search.FromTime, "min-time", ec.minTime)

		return nil, false
	}

	var res []Event

	for _, evt := range ec.events {
		matches := false

		// for the lower bound, ensure the event either ends after the it or, if there's no end time, start after it.
		if evt.EndTime != nil {
			matches = evt.EndTime.After(*search.FromTime)
		} else {
			matches = evt.StartTime.After(*search.FromTime)
		}

		// if we have an upper bound, ensure the event starts before that
		if search.ToTime != nil && evt.StartTime.After(*search.ToTime) {
			matches = false
		}

		if matches {
			if search.EventID != nil {
				if evt.ID == *search.EventID {
					ec.log.Debug("found event in cache", "event-id", *search.EventID)
					return []Event{evt}, true
				}
			} else {
				res = append(res, evt)
			}
		}
	}

	ec.log.Debug("loaded calendar events from cache", "count", len(res))

	return res, true
}

func PublishEvent(events eventsv1connect.EventServiceClient, msg proto.Message, retained bool) {
	go func() {
		pb, err := anypb.New(msg)
		if err != nil {
			slog.Error("failed to marshal protobuf message as anypb.Any", "error", err, "messageType", proto.MessageName(msg))
			return
		}

		if _, err := events.Publish(context.Background(), connect.NewRequest(&eventsv1.Event{
			Event:    pb,
			Retained: retained,
		})); err != nil {
			slog.Error("failed to publish event", "error", err, "messageType", proto.MessageName(msg))
		}
	}()
}
