package services

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
)

type timeRange [2]time.Time

func (tr timeRange) includes(t time.Time) bool {
	return (tr[0].Equal(t) || tr[0].Before(t)) && tr[1].After(t)
}

func calculateFreeSlots(calID string, start time.Time, end time.Time, events []repo.Event) ([]repo.Event, error) {
	// find all events that are within start/end
	filtered := make(repo.EventList, 0, len(events))

	// get all events that are within start and end.
	bounds := timeRange{start, end}
	for _, evt := range events {
		// skip full day events and events without an end date
		if evt.EndTime == nil || evt.FullDayEvent || evt.EndTime.IsZero() {
			slog.Info("skipping fullday event")
			continue
		}

		evtBounds := timeRange{evt.StartTime, *evt.EndTime}

		matches := bounds.includes(evt.StartTime) ||
			bounds.includes(*evt.EndTime) ||
			evtBounds.includes(start) ||
			evtBounds.includes(end)

		if matches {
			filtered = append(filtered, evt)
		}
	}

	// sort all filtered events
	sort.Sort(filtered)

	slog.Info("filtered events", "count", len(filtered))

	var slots repo.EventList
	for i := 0; i < len(filtered); i++ {
		var (
			startOfSlot time.Time
			endOfSlot   time.Time
		)

		if i == 0 {
			startOfSlot = start
		} else {
			startOfSlot = *filtered[i-1].EndTime
		}

		if i > 0 && filtered[i].StartTime.Before(filtered[i-1].StartTime) {
			return nil, fmt.Errorf("invalid slice sort")
		}

		if i == len(filtered) {
			endOfSlot = end
		} else {
			endOfSlot = filtered[i].StartTime
		}

		slog.Info("checking slot", "start", startOfSlot.Format("15:04"), "end", endOfSlot.Format("15:04"))

		if endOfSlot.Sub(startOfSlot) > time.Minute {
			slots = append(slots, repo.Event{
				CalendarID: calID,
				StartTime:  startOfSlot,
				EndTime:    &endOfSlot,
				ID:         "free-slot-" + strconv.Itoa(i),
				Summary:    "Freier Slot für " + endOfSlot.Sub(startOfSlot).String(),
			})
		}
	}

	if len(filtered) > 0 {
		if last := filtered[len(filtered)-1]; last.EndTime.Before(end) {
			slog.Info("found free slot at the end")

			slots = append(slots, repo.Event{
				ID:         "free-slot-end",
				CalendarID: calID,
				StartTime:  *last.EndTime,
				EndTime:    &end,
				Summary:    "Freier Slot für " + end.Sub(*last.EndTime).String(),
			})
		}
	}

	result := append(filtered, slots...)

	// sort the result
	sort.Sort(result)

	return result, nil
}
