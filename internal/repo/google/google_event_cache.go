package google

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	connect "github.com/bufbuild/connect-go"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	eventsv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1/eventsv1connect"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
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
	events       []repo.Event
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

	cache.wg.Add(1)

	go cache.watch(ctx)

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
		startOfCache := currentMidnight.AddDate(-1, 0, 0)

		ec.minTime = startOfCache

		call.ShowDeleted(false).SingleEvents(false).TimeMin(ec.minTime.Format(time.RFC3339))
	} else {
		call.SyncToken(ec.syncToken)
	}

	updatesProcessed := 0
	changeTypes := make(map[string]int)

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

			changeTypes[change]++

			req := &calendarv1.CalendarChangeEvent{
				Calendar: ec.calID,
			}

			switch change {
			case "deleted":
				req.Kind = &calendarv1.CalendarChangeEvent_DeletedEventId{
					DeletedEventId: item.Id,
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
		ec.log.Info("processed updates", "updates", updatesProcessed, "types", changeTypes)
	}

	sort.Sort(repo.ByStartTime(ec.events))

	return true
}

func (ec *googleEventCache) syncEvent(ctx context.Context, item *calendar.Event) (*repo.Event, string) {
	evt, err := googleEventToModel(ctx, ec.calID, item)
	if err != nil {
		ec.log.Error("failed to convert event", "event-id", item.Id, "error", err)
		return nil, ""
	}

	// this event has been deleted
	if item.Status == "cancelled" {
		ec.deleteEvent(item.Id)

		return nil, "deleted"
	}

	replaced := ec.replaceOrAppend(item.Id, *evt)
	if replaced {
		return evt, "updated"
	}

	return evt, "created"
}

func (ec *googleEventCache) deleteEvent(id string) bool {
	newEvents := slices.DeleteFunc(ec.events, func(e repo.Event) bool {
		return e.ID == id
	})

	oldLen := len(ec.events)
	ec.events = newEvents

	deleted := oldLen != len(newEvents)
	if deleted {
		ec.log.Info("deleted event", "id", id)
	} else {
		ec.log.Warn("failed to delete event", "id", id)
	}

	return deleted
}

func (ec *googleEventCache) replaceEvent(id string, newModel repo.Event) bool {
	idx := slices.IndexFunc(ec.events, func(e repo.Event) bool {
		return e.ID == id
	})

	if idx < 0 {
		return false
	}

	ec.events[idx] = newModel
	return true
}

func (ec *googleEventCache) replaceOrAppend(id string, newModel repo.Event) bool {
	if ec.replaceEvent(id, newModel) {
		return true
	}

	ec.events = append(ec.events, newModel)
	return false
}

func (ec *googleEventCache) tryLoadFromCache(ctx context.Context, search *repo.EventSearchOptions) ([]repo.Event, bool) {
	// check if it's even possible to serve the request from cache.
	if search == nil {
		return nil, false
	}
	if search.FromTime == nil {
		return nil, false
	}

	ec.rw.RLock()
	defer ec.rw.RUnlock()

	if search.FromTime.Before(ec.minTime) && !ec.minTime.IsZero() {
		ec.log.Info("not using cache: search.from is before minTime", "search-time", search.FromTime, "min-time", ec.minTime)

		return nil, false
	}

	var res []repo.Event

	for _, evt := range ec.events {
		matches := repo.EventMatches(evt, search)

		switch {
		case matches && search.EventID != nil:
			ec.log.Debug("found event in cache", "event-id", *search.EventID)
			return []repo.Event{evt}, true
		case matches:
			res = append(res, evt)
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
