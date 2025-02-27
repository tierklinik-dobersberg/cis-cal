package cmds

import "github.com/tierklinik-dobersberg/apis/pkg/cli"

func PrepareRootCommand(root *cli.Root) {
	root.AddCommand(
		GetCalendarCommand(root),
		GetEventsCommand(root),
		GetHolidayCommand(root),
		GetResourceCommand(root),
	)
}
