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
	"github.com/sirupsen/logrus"
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
	location      *time.Location
	firstLoadDone chan struct{}
	trigger       chan struct{}

	calID        string
	events       []Event
	svc          *calendar.Service
	eventService eventsv1connect.EventServiceClient
}

func (ec *googleEventCache) String() string {
	return fmt.Sprintf("Cache<%s>", ec.calID)
}

// nolint:unparam
func newCache(ctx context.Context, id string, loc *time.Location, svc *calendar.Service, eventCli eventsv1connect.EventServiceClient) (*googleEventCache, error) {
	cache := &googleEventCache{
		calID:         id,
		svc:           svc,
		location:      loc,
		firstLoadDone: make(chan struct{}),
		trigger:       make(chan struct{}),
		eventService:  eventCli,
	}

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

		if firstLoad {
			firstLoad = false
			close(ec.firstLoadDone)
		}

		ec.evictFromCache(ctx)

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
		now := time.Now()
		ec.minTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ec.location)
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

			logrus.Errorf("failed to sync events: %s", err)

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
					slog.Error("failed to convert event to protobuf", "error", err)
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
		logrus.Errorf("unexpected google api response, starting over")
		ec.syncToken = ""
		ec.events = nil
		ec.minTime = time.Time{}

		return false
	}
	if updatesProcessed > 0 {
		logrus.Infof("processed %d updates", updatesProcessed)
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
			logrus.Errorf("failed to convert event: %s", err)
			return nil, ""
		}
		ec.events[foundAtIndex] = *evt

		return evt, "updated"
	}

	evt, err := googleEventToModel(ctx, ec.calID, item)
	if err != nil {
		logrus.Errorf("failed to convert event: %s", err)
		return nil, ""
	}
	ec.events = append(ec.events, *evt)

	return evt, "created"
}

func (ec *googleEventCache) evictFromCache(ctx context.Context) {
	ec.rw.Lock()
	defer ec.rw.Unlock()

	// TODO(ppacher): make cache limit configurable
	const threshold = 200

	if len(ec.events) < threshold {
		return
	}
	evictLimit := len(ec.events) - threshold

	now := time.Now()
	currentMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ec.location)

	var idx int
	for idx = range ec.events {
		if ec.events[idx].StartTime.After(currentMidnight) {
			break
		}
		if idx > evictLimit {
			break
		}
	}

	if idx == 0 {
		logrus.Infof("cannot evict cache entries for today.")
		return
	}

	ec.events = ec.events[idx:]
	ec.minTime = ec.events[0].StartTime
	logrus.Infof("evicted %d events from cache which now starts with %s and holds %d events", idx, ec.minTime.Format(time.RFC3339), len(ec.events))
}

func (ec *googleEventCache) tryLoadFromCache(ctx context.Context, search *EventSearchOptions) ([]Event, bool) {
	// check if it's even possible to serve the request from cache.
	if search == nil {
		logrus.Infof("not using cache: search == nil")
		return nil, false
	}
	if search.FromTime == nil {
		logrus.Infof("not using cache: search.from == nil")
		return nil, false
	}

	ec.rw.RLock()
	defer ec.rw.RUnlock()
	if search.FromTime.Before(ec.minTime) && !ec.minTime.IsZero() {
		logrus.Infof("not using cache: search.from (%s) is before minTime (%s)", search.FromTime, ec.minTime)

		return nil, false
	}

	var res []Event

	for _, evt := range ec.events {
		startInRange := search.FromTime.Equal(evt.StartTime) || search.FromTime.Before(evt.StartTime)
		if search.ToTime != nil {
			startInRange = startInRange && (search.ToTime.Equal(evt.StartTime) || search.ToTime.After(evt.StartTime))
		}

		if startInRange {
			if search.EventID != nil {
				if evt.ID == *search.EventID {
					logrus.Infof("found event with id %q in cache", *search.EventID)
					return []Event{evt}, true
				}
			} else {
				res = append(res, evt)
			}
		}
	}

	logrus.Infof("loaded %d calendar events from cache", len(res))
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
