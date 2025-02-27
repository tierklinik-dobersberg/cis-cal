package resources

import calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"

type ResourceCalendar struct {
	Name             string `bson:"name"`
	DisplayName      string `bson:"displayName"`
	Description      string `bson:"description"`
	Color            string `bson:"color"`
	MaxConcurrentUse uint32 `bson:"maxConcurrentUse"`
}

func (r ResourceCalendar) ToProto() *calendarv1.ResourceCalendar {
	display := r.DisplayName
	if display == "" {
		display = r.Name
	}

	use := r.MaxConcurrentUse
	if r.MaxConcurrentUse == 0 {
		use = 1
	}

	return &calendarv1.ResourceCalendar{
		Name:             r.Name,
		DisplayName:      display,
		MaxConcurrentUse: use,
		Color:            r.Color,
	}
}
