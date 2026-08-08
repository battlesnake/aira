package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"aira/internal/domain"
)

// SetTicket applies the CLI's field=value mutation vocabulary. The selector
// is resolved by the caller so this method only accepts an exact ticket ID.
func (s *Store) SetTicket(ctx context.Context, id, field, value string) (EventKey, error) {
	field = strings.ToLower(strings.TrimSpace(field))
	value = strings.TrimSpace(value)
	if value == "" && field != "body" && field != "title" {
		return EventKey{}, errors.New("E_CONFIG_INVALID: empty set value")
	}
	return s.UpdateTicketContent(ctx, id, func(ticket domain.Ticket, body string) (domain.Ticket, string, error) {
		switch field {
		case "title":
			ticket.Title = value
		case "kind":
			ticket.Kind = domain.Kind(value)
		case "severity":
			ticket.Severity = domain.Severity(value)
		case "status":
			ticket.Status = domain.Status(value)
		case "hold":
			hold, err := strconv.ParseBool(strings.ToLower(value))
			if err != nil {
				return domain.Ticket{}, "", errors.New("E_CONFIG_INVALID: hold must be true or false")
			}
			ticket.Hold = hold
		case "label":
			found := false
			for _, label := range ticket.Labels {
				if label == value {
					found = true
				}
			}
			if !found {
				ticket.Labels = append(ticket.Labels, value)
			}
		case "labels":
			labels := strings.Split(value, ",")
			ticket.Labels = labels
		case "body":
			body = value
		default:
			return domain.Ticket{}, "", errors.New("E_CONFIG_INVALID: unsupported ticket field")
		}
		return ticket, body, nil
	})
}

func (s *Store) MoveTicket(ctx context.Context, id string, status domain.Status) (EventKey, error) {
	return s.UpdateTicketContent(ctx, id, func(ticket domain.Ticket, body string) (domain.Ticket, string, error) {
		if err := domain.ValidateTransition(ticket.Status, status); err != nil {
			return domain.Ticket{}, "", err
		}
		ticket.Status = status
		return ticket, body, nil
	})
}
