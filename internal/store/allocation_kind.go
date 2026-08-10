package store

import (
	"path/filepath"
	"sort"
	"strings"
)

// Entity kinds for ID allocation. A prefix belongs to exactly one kind, and the
// prefix registry is the authority for a prefix's kind; the kind is carried
// redundantly on allocation rows, receipts, events, and paths only so it can be
// recovered and cross-validated after DB loss.
const (
	kindTicket      = "ticket"
	kindRequirement = "requirement"
)

func validAllocationKind(kind string) bool {
	return kind == kindTicket || kind == kindRequirement
}

// normaliseKind maps an empty (pre-M9, missing) kind to the ticket default; any
// other non-empty value is returned as-is so an invalid value is caught, not
// silently coerced.
func normaliseKind(kind string) string {
	if kind == "" {
		return kindTicket
	}
	return kind
}

// entityPathForKind returns the git-file path where an allocation of the given
// kind materialises. Kind is authoritative for the path, so a requirement never
// lands under .aira/tickets/ and a ticket never under .aira/requirements/.
func (s *Store) entityPathForKind(kind, id string) string {
	if kind == kindRequirement {
		return s.requirementPath(id)
	}
	return s.ticketPath(id)
}

func (s *Store) requirementPath(id string) string {
	return filepath.Join(s.root, ".aira", "requirements", id+".md")
}

// kindForPath derives the entity kind implied by an allocation path, or "" if
// the path is under neither entity directory. Used to cross-validate a durable
// receipt/allocation path against the authoritative prefix kind.
func kindForPath(path string) string {
	normalised := filepath.ToSlash(path)
	switch {
	case strings.Contains(normalised, ".aira/requirements/"):
		return kindRequirement
	case strings.Contains(normalised, ".aira/tickets/"):
		return kindTicket
	default:
		return ""
	}
}

// prefixesByKind returns the sorted prefix names registered under the given kind.
func (s *Store) prefixesByKind(kind string) []string {
	result := make([]string, 0, len(s.prefixes))
	for prefix, prefixKind := range s.prefixes {
		if prefixKind == kind {
			result = append(result, prefix)
		}
	}
	sort.Strings(result)
	return result
}
