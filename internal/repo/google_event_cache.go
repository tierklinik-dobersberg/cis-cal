package repo

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

type googleEventCache struct {
	rw            sync.RWMutex
	minTime       time.Time
	syncToken     string
	location      *time.Location
	firstLoadDone chan struct{}
	trigger       chan struct{}

	calID  string
	events []Event
	svc    *calendar.Service
}

func (ec *googleEventCache) String() string {
	return fmt.Sprintf("Cache<%s>", ec.calID)
}

// nolint:unparam
func newCache(ctx context.Context, id string, loc *time.Location, svc *calendar.Service) (*googleEventCache, error) {
	cache := &googleEventCache{
		calID:         id,
		svc:           svc,
		location:      loc,
		firstLoadDone: make(chan struct{}),
		trigger:       make(chan struct{}),
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
		success := ec.loadEvents(ctx, !firstLoad)
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

func (ec *googleEventCache) loadEvents(ctx context.Context, emit bool) bool {
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

		res, err := call.Do()
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
			ec.syncAndEmit(ctx, item, emit)
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

func (ec *googleEventCache) syncAndEmit(ctx context.Context, item *calendar.Event, emit bool) {
	_, _ = ec.syncEvent(ctx, item)
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
		evt, err := convertToEvent(ctx, ec.calID, item)
		if err != nil {
			logrus.Errorf("failed to convert event: %s", err)
			return nil, ""
		}
		ec.events[foundAtIndex] = *evt

		return evt, "updated"
	}

	evt, err := convertToEvent(ctx, ec.calID, item)
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
			res = append(res, evt)
		}
	}

	logrus.Infof("loaded %d calendar events from cache", len(res))
	return res, true
}
