package repo

func EventMatches(evt Event, search *EventSearchOptions) bool {
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

	if search.EventID != nil && evt.ID != *search.EventID {
		matches = false
	}

	return matches
}
