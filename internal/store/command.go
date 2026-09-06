package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"aira/internal/domain"
)

type CommandEventAddResult struct {
	Event        domain.CommandEvent `json:"event"`
	ID           string              `json:"id"`
	EvictedCount int                 `json:"evicted_count"`
	Remaining    int                 `json:"remaining"`
}

type CommandDistributionGroup struct {
	Value         string                  `json:"value"`
	KeySource     domain.CommandKeySource `json:"key_source,omitempty"`
	Key           string                  `json:"key,omitempty"`
	Count         int                     `json:"count"`
	Exited        int                     `json:"exited"`
	ExitedNonzero int                     `json:"exited_nonzero"`
	Signalled     int                     `json:"signalled"`
	Timeout       int                     `json:"timeout"`
	LaunchFailed  int                     `json:"launch_failed"`
	Unknown       int                     `json:"unknown"`
}

type CommandDistributionResult struct {
	By     string                     `json:"by"`
	Total  int                        `json:"total"`
	Groups []CommandDistributionGroup `json:"groups"`
}

type CommandLatencySummary struct {
	KeySource     domain.CommandKeySource `json:"key_source"`
	Key           string                  `json:"key"`
	Count         int                     `json:"count"`
	Exited        int                     `json:"exited"`
	ExitedNonzero int                     `json:"exited_nonzero"`
	Signalled     int                     `json:"signalled"`
	Timeout       int                     `json:"timeout"`
	LaunchFailed  int                     `json:"launch_failed"`
	Unknown       int                     `json:"unknown"`
	P50MS         *int64                  `json:"p50_ms,omitempty"`
	P95MS         *int64                  `json:"p95_ms,omitempty"`
	AtSeq         int64                   `json:"at_seq"`
}

func (s *Store) AddCommandEvent(ctx context.Context, input domain.CommandEventInput) (CommandEventAddResult, error) {
	input.At = strings.TrimSpace(input.At)
	input.Key = strings.TrimSpace(input.Key)
	input.Program = strings.TrimSpace(input.Program)
	input.Signal = strings.TrimSpace(input.Signal)
	input.TicketID = strings.TrimSpace(input.TicketID)
	input.Phase = strings.TrimSpace(input.Phase)
	input.Actor = strings.TrimSpace(input.Actor)
	input.Session = strings.TrimSpace(input.Session)
	input.Cwd = strings.TrimSpace(input.Cwd)
	if err := input.Validate(); err != nil {
		return CommandEventAddResult{}, err
	}
	if input.At == "" {
		input.At = timeNow()
	}
	input.GitContext = s.crossCheckGitContext(input.GitContext)
	var result CommandEventAddResult
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		number, sequence, err := nextCommandNumbers(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		event := domain.CommandEvent{
			ID: fmt.Sprintf("CMD-%d", number), At: input.At, AtSeq: sequence,
			Key: input.Key, KeySource: input.KeySource, Program: input.Program,
			ArgvPreview: input.ArgvPreview, ArgvDigest: input.ArgvDigest, PrefixPreview: input.PrefixPreview,
			Status: input.Status, ExitCode: cloneInt64(input.ExitCode), Signal: input.Signal, WallMS: cloneInt64(input.WallMS),
			TicketID: input.TicketID, Phase: input.Phase, Actor: input.Actor, Session: input.Session, Cwd: input.Cwd,
			GitContext: domain.CommandGitContextFrom(input.GitContext),
		}
		git := event.GitContext
		_, err = conn.ExecContext(ctx, `INSERT INTO command_events(project_id,id,at,at_seq,key,key_source,program,argv_preview,argv_digest,prefix_preview,status,exit_code,signal,wall_ms,ticket_id,phase,actor,session,cwd,head_hash,head_hash_status,head_ref,head_ref_status,worktree_id,worktree_id_status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.projectID, event.ID, event.At, event.AtSeq, event.Key, event.KeySource, event.Program,
			event.ArgvPreview, event.ArgvDigest, event.PrefixPreview, event.Status, optionalInt64(event.ExitCode), event.Signal,
			optionalInt64(event.WallMS), event.TicketID, event.Phase, event.Actor, event.Session, event.Cwd,
			git.HeadHash.Value, git.HeadHash.Status, git.HeadRef.Value, git.HeadRef.Status, git.WorktreeID.Value, git.WorktreeID.Status)
		if err != nil {
			return translateDBError(err)
		}
		evicted, err := s.evictCommandEvents(ctx, conn)
		if err != nil {
			return err
		}
		var remaining int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM command_events WHERE project_id=?`, s.projectID).Scan(&remaining); err != nil {
			return err
		}
		result = CommandEventAddResult{Event: event, ID: event.ID, EvictedCount: evicted, Remaining: remaining}
		return nil
	})
	return result, err
}

func nextCommandNumbers(ctx context.Context, conn *sql.Conn, project string) (int64, int64, error) {
	var number, sequence int64
	err := conn.QueryRowContext(ctx, `SELECT next_number,next_seq FROM command_event_counter WHERE project_id=?`, project).Scan(&number, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO command_event_counter(project_id,next_number,next_seq) VALUES(?,?,?)`, project, 2, 2); err != nil {
			return 0, 0, err
		}
		return 1, 1, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if number < 1 || sequence < 1 {
		// Not E_COMMAND_INVALID (AIRA-107): that code is the caller-facing
		// "your command-language program is malformed" answer, catalogued at
		// exit 2, and nothing about this failure is the caller's request. The
		// counter row is state AIRA itself wrote and can no longer trust, so it
		// is an infrastructure failure (exit 4) and must not send an agent off
		// to fix an argument that was fine.
		return 0, 0, errors.New("E_COMMAND_COUNTER_CORRUPT: command event counter is invalid")
	}
	if _, err := conn.ExecContext(ctx, `UPDATE command_event_counter SET next_number=?,next_seq=? WHERE project_id=?`, number+1, sequence+1, project); err != nil {
		return 0, 0, err
	}
	return number, sequence, nil
}

func (s *Store) evictCommandEvents(ctx context.Context, conn *sql.Conn) (int, error) {
	result, err := conn.ExecContext(ctx, `DELETE FROM command_events WHERE project_id=? AND at_seq NOT IN (SELECT at_seq FROM command_events WHERE project_id=? ORDER BY at_seq DESC LIMIT ?)`, s.projectID, s.projectID, s.maxCommandEvents)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	evicted := int(count)
	if s.maxCommandAgeDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.maxCommandAgeDays).Format(time.RFC3339Nano)
		result, err = conn.ExecContext(ctx, `DELETE FROM command_events WHERE project_id=? AND at<?`, s.projectID, cutoff)
		if err != nil {
			return 0, err
		}
		count, err = result.RowsAffected()
		if err != nil {
			return 0, err
		}
		evicted += int(count)
	}
	return evicted, nil
}

type commandFilter struct{ field, value string }

var commandFilterFields = []string{"program", "key-source", "key", "status", "branch", "commit", "ticket", "phase"}

func commandFilters(query string) ([]commandFilter, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	var filters []commandFilter
	for len(query) > 0 {
		field := ""
		for _, candidate := range commandFilterFields {
			if strings.HasPrefix(query, candidate+":") {
				field = candidate
				break
			}
		}
		if field == "" {
			return nil, fmt.Errorf("E_SELECTOR_INVALID: invalid command query near %q", query)
		}
		query = query[len(field)+1:]
		next := len(query)
		// Keys are deliberately allowed to contain spaces and filter-looking
		// text. Paired gauge drills put key last, so consume the complete tail
		// and keep those drilldowns exactly resolvable.
		if field != "key" {
			for _, candidate := range commandFilterFields {
				if at := strings.Index(query, " "+candidate+":"); at >= 0 && at < next {
					next = at
				}
			}
		}
		value := strings.TrimSpace(query[:next])
		if value == "" {
			return nil, fmt.Errorf("E_SELECTOR_INVALID: empty command query field %q", field)
		}
		filters = append(filters, commandFilter{field: field, value: value})
		if next == len(query) {
			break
		}
		query = strings.TrimSpace(query[next+1:])
	}
	return filters, nil
}

const commandSelect = `SELECT id,at,at_seq,key,key_source,program,argv_preview,argv_digest,prefix_preview,status,exit_code,signal,wall_ms,ticket_id,phase,actor,session,cwd,head_hash,head_hash_status,head_ref,head_ref_status,worktree_id,worktree_id_status FROM command_events`

func commandWhere(project string, filters []commandFilter) (string, []any) {
	where, values := "project_id=?", []any{project}
	for _, filter := range filters {
		switch filter.field {
		case "program":
			where += " AND program=?"
			values = append(values, filter.value)
		case "key-source":
			where += " AND key_source=?"
			values = append(values, filter.value)
		case "key":
			where += " AND key=?"
			values = append(values, filter.value)
		case "status":
			where += " AND status=?"
			values = append(values, filter.value)
		case "ticket":
			where += " AND ticket_id=?"
			values = append(values, filter.value)
		case "phase":
			where += " AND phase=?"
			values = append(values, filter.value)
		case "commit":
			where += " AND head_hash_status='value' AND head_hash=?"
			values = append(values, filter.value)
		case "branch":
			where += " AND head_ref_status='value'"
			if strings.HasPrefix(filter.value, "refs/") {
				where += " AND head_ref=?"
				values = append(values, filter.value)
			} else {
				where += " AND (head_ref=? OR head_ref=?)"
				values = append(values, filter.value, "refs/heads/"+filter.value)
			}
		}
	}
	return where, values
}

func (s *Store) ListCommandEvents(query string) ([]domain.CommandEvent, error) {
	filters, err := commandFilters(query)
	if err != nil {
		return nil, err
	}
	where, values := commandWhere(s.projectID, filters)
	rows, err := s.db.Query(commandSelect+` WHERE `+where+` ORDER BY at_seq DESC`, values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.CommandEvent{}
	for rows.Next() {
		event, err := scanCommandEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func scanCommandEvent(row interface{ Scan(...any) error }) (domain.CommandEvent, error) {
	var event domain.CommandEvent
	var exitCode, wall sql.NullInt64
	if err := row.Scan(&event.ID, &event.At, &event.AtSeq, &event.Key, &event.KeySource, &event.Program, &event.ArgvPreview, &event.ArgvDigest, &event.PrefixPreview, &event.Status, &exitCode, &event.Signal, &wall, &event.TicketID, &event.Phase, &event.Actor, &event.Session, &event.Cwd, &event.GitContext.HeadHash.Value, &event.GitContext.HeadHash.Status, &event.GitContext.HeadRef.Value, &event.GitContext.HeadRef.Status, &event.GitContext.WorktreeID.Value, &event.GitContext.WorktreeID.Status); err != nil {
		return domain.CommandEvent{}, err
	}
	event.ExitCode, event.WallMS = nullInt64(exitCode), nullInt64(wall)
	return event, nil
}

func (s *Store) CommandDistribution(query, by string) (CommandDistributionResult, error) {
	valid := map[string]bool{"program": true, "key": true, "status": true, "branch": true, "ticket": true}
	if !valid[by] {
		return CommandDistributionResult{}, fmt.Errorf("E_SELECTOR_INVALID: unsupported command distribution field %q", by)
	}
	filters, err := commandFilters(query)
	if err != nil {
		return CommandDistributionResult{}, err
	}
	where, values := commandWhere(s.projectID, filters)
	valueExpr, sourceExpr, keyExpr, groupExpr := "program", "''", "''", "program"
	switch by {
	case "key":
		valueExpr, sourceExpr, keyExpr, groupExpr = "key", "key_source", "key", "key_source,key"
	case "status":
		valueExpr, groupExpr = "status", "status"
	case "ticket":
		valueExpr, groupExpr = "CASE WHEN ticket_id='' THEN '(none)' ELSE ticket_id END", "CASE WHEN ticket_id='' THEN '(none)' ELSE ticket_id END"
	case "branch":
		valueExpr, groupExpr = "CASE WHEN head_ref_status='value' THEN CASE WHEN head_ref LIKE 'refs/heads/%' THEN substr(head_ref,12) ELSE head_ref END ELSE '('||head_ref_status||')' END", "CASE WHEN head_ref_status='value' THEN CASE WHEN head_ref LIKE 'refs/heads/%' THEN substr(head_ref,12) ELSE head_ref END ELSE '('||head_ref_status||')' END"
	}
	querySQL := `SELECT ` + valueExpr + `,` + sourceExpr + `,` + keyExpr + `,COUNT(*),SUM(status='exited'),SUM(status='exited' AND exit_code<>0),SUM(status='signalled'),SUM(status='timeout'),SUM(status='launch-failed'),SUM(status='unknown') FROM command_events WHERE ` + where + ` GROUP BY ` + groupExpr + ` ORDER BY 1,2`
	rows, err := s.db.Query(querySQL, values...)
	if err != nil {
		return CommandDistributionResult{}, err
	}
	defer rows.Close()
	result := CommandDistributionResult{By: by, Groups: []CommandDistributionGroup{}}
	for rows.Next() {
		var group CommandDistributionGroup
		if err := rows.Scan(&group.Value, &group.KeySource, &group.Key, &group.Count, &group.Exited, &group.ExitedNonzero, &group.Signalled, &group.Timeout, &group.LaunchFailed, &group.Unknown); err != nil {
			return CommandDistributionResult{}, err
		}
		result.Total += group.Count
		result.Groups = append(result.Groups, group)
	}
	if err := rows.Err(); err != nil {
		return CommandDistributionResult{}, err
	}
	return result, nil
}

func (s *Store) CommandLatencyByKeyPair(ctx context.Context) ([]CommandLatencySummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_source,key,status,exit_code,wall_ms,at_seq FROM command_events WHERE project_id=? ORDER BY key_source,key,wall_ms`, s.projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type aggregate struct {
		summary   CommandLatencySummary
		durations []int64
	}
	groups := map[string]*aggregate{}
	for rows.Next() {
		var source domain.CommandKeySource
		var key string
		var status domain.CommandOutcome
		var exit, wall sql.NullInt64
		var seq int64
		if err := rows.Scan(&source, &key, &status, &exit, &wall, &seq); err != nil {
			return nil, err
		}
		identity := string(source) + "\x00" + key
		group := groups[identity]
		if group == nil {
			group = &aggregate{summary: CommandLatencySummary{KeySource: source, Key: key}}
			groups[identity] = group
		}
		group.summary.Count++
		if seq > group.summary.AtSeq {
			group.summary.AtSeq = seq
		}
		switch status {
		case domain.CommandExited:
			group.summary.Exited++
			if exit.Valid && exit.Int64 != 0 {
				group.summary.ExitedNonzero++
			}
			if wall.Valid {
				group.durations = append(group.durations, wall.Int64)
			}
		case domain.CommandSignalled:
			group.summary.Signalled++
		case domain.CommandTimeout:
			group.summary.Timeout++
		case domain.CommandLaunchFailed:
			group.summary.LaunchFailed++
		case domain.CommandUnknown:
			group.summary.Unknown++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]CommandLatencySummary, 0, len(groups))
	for _, group := range groups {
		sort.Slice(group.durations, func(i, j int) bool { return group.durations[i] < group.durations[j] })
		if len(group.durations) >= 5 {
			value := nearestRank(group.durations, 50)
			group.summary.P50MS = &value
		}
		if len(group.durations) >= 20 {
			value := nearestRank(group.durations, 95)
			group.summary.P95MS = &value
		}
		result = append(result, group.summary)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].KeySource == result[j].KeySource {
			return result[i].Key < result[j].Key
		}
		return result[i].KeySource < result[j].KeySource
	})
	return result, nil
}

func nearestRank(sorted []int64, percentile int) int64 {
	rank := (percentile*len(sorted) + 99) / 100
	return sorted[rank-1]
}
