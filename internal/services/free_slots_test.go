package services

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
)

func makeTime(ts string) time.Time {
	t, err := time.Parse("15:04", ts)
	if err != nil {
		panic(err)
	}

	return time.Date(2000, time.January, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func makeRange(start, end string) timeRange {
	return timeRange{makeTime(start), makeTime(end)}
}

func Test_FreeSlots(t *testing.T) {
	cases := []struct {
		Range  timeRange
		Events []timeRange
		Slots  []timeRange
	}{
		{
			makeRange("06:00", "12:00"),
			[]timeRange{
				makeRange("06:00", "06:30"),
			},
			[]timeRange{
				makeRange("06:30", "12:00"),
			},
		},
		{
			makeRange("06:00", "12:00"),
			[]timeRange{
				makeRange("08:00", "12:30"),
			},
			[]timeRange{
				makeRange("06:00", "08:00"),
			},
		},
		{
			makeRange("06:00", "12:00"),
			[]timeRange{
				makeRange("06:00", "06:00"),
				makeRange("07:00", "08:45"),
				makeRange("06:00", "06:30"),
			},
			[]timeRange{
				makeRange("06:30", "07:00"),
				makeRange("08:45", "12:00"),
			},
		},
		{
			makeRange("06:00", "12:00"),
			[]timeRange{
				makeRange("05:00", "12:30"),
			},
			[]timeRange{},
		},
		{
			makeRange("12:00", "14:00"),
			[]timeRange{
				makeRange("06:00", "06:30"),
				makeRange("14:00", "15:00"),
			},
			[]timeRange{
				makeRange("12:00", "14:00"),
			},
		},
	}

	for _, c := range cases {
		events := make([]repo.Event, 0, len(c.Events))
		for _, e := range c.Events {
			events = append(events, repo.Event{
				StartTime: e[0],
				EndTime:   &e[1],
			})
		}

		result, err := calculateFreeSlots("", c.Range[0], c.Range[1], events)
		require.NoError(t, err)

		slots := make([]timeRange, 0, len(result))
		for _, e := range result {
			if e.ID != "" {
				slots = append(slots, timeRange{e.StartTime, *e.EndTime})
			}
		}

		assert.Equal(t, c.Slots, slots)
	}
}
