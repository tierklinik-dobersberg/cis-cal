package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bufbuild/connect-go"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1/calendarv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/log"
)

type HolidayService struct {
	calendarv1connect.UnimplementedHolidayServiceHandler

	country string
	getter  HolidayGetter
}

func NewHolidayService(country string) *HolidayService {
	getter := NewHolidayCache()

	return &HolidayService{
		country: country,
		getter:  getter,
	}
}

func (svc *HolidayService) GetHoliday(ctx context.Context, req *connect.Request[calendarv1.GetHolidayRequest]) (*connect.Response[calendarv1.GetHolidayResponse], error) {
	holidays, err := svc.getter.Get(ctx, svc.country, int(req.Msg.GetYear()))
	if err != nil {
		return nil, err
	}

	prefix := fmt.Sprintf("%d-", req.Msg.GetYear())
	if req.Msg.Month > 0 {
		prefix += fmt.Sprintf("%02d-", req.Msg.GetMonth())
	}

	result := make([]*calendarv1.PublicHoliday, 0, len(holidays))
	for _, p := range holidays {
		if !strings.HasPrefix(p.Date, prefix) {
			continue
		}

		var protoType calendarv1.HolidayType

		switch p.Type {
		case "Public":
			protoType = calendarv1.HolidayType_PUBLIC
		case "Bank":
			protoType = calendarv1.HolidayType_BANK
		case "School":
			protoType = calendarv1.HolidayType_SCHOOL
		case "Authorities":
			protoType = calendarv1.HolidayType_AUTHORITIES
		case "Optional":
			protoType = calendarv1.HolidayType_OPTIONAL
		case "Observance":
			protoType = calendarv1.HolidayType_OBSERVANCE
		default:
			log.L(ctx).Errorf("unsupported public holiday type %q", p.Type)

			protoType = calendarv1.HolidayType_HOLIDAY_TYPE_UNSPECIFIED
		}

		result = append(result, &calendarv1.PublicHoliday{
			Date:        p.Date,
			LocalName:   p.LocalName,
			Name:        p.Name,
			CountryCode: p.CountryCode,
			Fixed:       p.Fixed,
			Global:      p.Global,
			Type:        protoType,
		})
	}

	return connect.NewResponse(&calendarv1.GetHolidayResponse{
		Holidays: result,
	}), nil
}

func (svc *HolidayService) NumberOfWorkDays(ctx context.Context, req *connect.Request[calendarv1.NumberOfWorkDaysRequest]) (*connect.Response[calendarv1.NumberOfWorkDaysResponse], error) {
	from := req.Msg.From.AsTime()
	to := req.Msg.To.AsTime()

	response := &calendarv1.NumberOfWorkDaysResponse{}

	country := req.Msg.Country
	if country == "" {
		country = svc.country
	}

L:
	for iter := from; iter.Before(to) || iter.Equal(to); iter = iter.AddDate(0, 0, 1) {
		switch iter.Weekday() {
		case time.Saturday, time.Sunday:
			response.NumberOfWeekendDays++
			continue
		default:
			isHoliday, err := svc.getter.IsHoliday(ctx, country, iter)
			if err != nil {
				break L
			}

			if isHoliday {
				response.NumberOfHolidays++
			} else {
				response.NumberOfWorkDays++
			}
		}
	}

	return connect.NewResponse(response), nil
}
