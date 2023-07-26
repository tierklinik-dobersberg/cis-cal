package cmds

import (
	"context"

	"github.com/bufbuild/connect-go"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1/calendarv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
)

func GetCalendarCommand(root *cli.Root) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "calendar",
		Aliases: []string{"calendars", "cal"},
		Run: func(cmd *cobra.Command, args []string) {
			cli := calendarv1connect.NewCalendarServiceClient(root.HttpClient, root.BaseURL)

			calendars, err := cli.ListCalendars(context.Background(), connect.NewRequest(&calendarv1.ListCalendarsRequest{}))
			if err != nil {
				logrus.Fatalf("failed to get calendar list: %s", err)
			}

			root.Print(calendars.Msg)
		},
	}

	return cmd
}
