package cmds

import (
	"context"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1/calendarv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
)

func GetHolidayCommand(root *cli.Root) *cobra.Command {
	var (
		year  int
		month int
	)
	cmd := &cobra.Command{
		Use:     "holiday",
		Aliases: []string{"holidays"},
		Run: func(cmd *cobra.Command, args []string) {
			cli := calendarv1connect.NewHolidayServiceClient(root.HttpClient, root.BaseURL)

			if year == 0 {
				year = time.Now().Year()
			}

			req := &calendarv1.GetHolidayRequest{
				Year:  uint64(year),
				Month: uint64(month),
			}

			res, err := cli.GetHoliday(context.Background(), connect.NewRequest(req))

			if err != nil {
				logrus.Fatalf("failed to get holidays: %s", err)
			}

			root.Print(res.Msg)
		},
	}

	cmd.Flags().IntVar(&year, "year", 0, "The year to query holidays")
	cmd.Flags().IntVar(&month, "month", 0, "The month to query holidays")

	return cmd
}
