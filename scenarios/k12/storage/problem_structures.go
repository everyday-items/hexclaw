package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type problemStructureMember struct {
	ProblemID         string   `json:"problem_id"`
	Ordinal           int      `json:"ordinal"`
	ProblemKind       string   `json:"problem_kind"`
	ParentProblemID   string   `json:"parent_problem_id"`
	SubproblemNo      string   `json:"subproblem_no"`
	SourceNumberPath  []string `json:"source_number_path"`
	DisplayLabel      string   `json:"display_label"`
	DependencyGroupID string   `json:"dependency_group_id"`
	InputRevision     int      `json:"-"`
}

type currentProblemStructure struct {
	Version      int
	Digest       string
	MappingState string
	Members      map[string]problemStructureMember
}

func dependencyGroupID(problem k12.Problem) string {
	if problem.ProblemKind == k12.ProblemKindSubproblem {
		return "parent:" + problem.ParentProblemID
	}
	return "problem:" + problem.ProblemID
}

func problemStructureFacts(
	snapshot k12.ProblemAttemptSnapshot,
) ([]problemStructureMember, string, error) {
	attemptRevision := make(map[string]int, len(snapshot.Attempts))
	for _, attempt := range snapshot.Attempts {
		revision := attempt.ConfirmedVersion
		if revision < 1 {
			revision = 1
		}
		attemptRevision[attempt.ProblemID] = revision
	}
	members := make([]problemStructureMember, 0, len(snapshot.Problems))
	for _, problem := range snapshot.Problems {
		revision := attemptRevision[problem.ProblemID]
		if revision < 1 {
			revision = 1
		}
		members = append(members, problemStructureMember{
			ProblemID:         problem.ProblemID,
			Ordinal:           problem.Ordinal,
			ProblemKind:       problem.ProblemKind,
			ParentProblemID:   problem.ParentProblemID,
			SubproblemNo:      problem.SubproblemNo,
			SourceNumberPath:  append([]string(nil), problem.SourceNumberPath...),
			DisplayLabel:      problem.DisplayLabel,
			DependencyGroupID: dependencyGroupID(problem),
			InputRevision:     revision,
		})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Ordinal == members[j].Ordinal {
			return members[i].ProblemID < members[j].ProblemID
		}
		return members[i].Ordinal < members[j].Ordinal
	})
	raw, err := json.Marshal(members)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return members, hex.EncodeToString(sum[:]), nil
}

func getCurrentProblemStructureTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	submissionID string,
) (currentProblemStructure, error) {
	var current currentProblemStructure
	err := tx.QueryRowContext(ctx, `
		SELECT structure_version,structure_digest,mapping_state
		FROM k12_problem_structure_snapshots
		WHERE agent_name=? AND submission_id=?
		  AND current_disposition='current'`,
		agentName,
		submissionID,
	).Scan(&current.Version, &current.Digest, &current.MappingState)
	if err != nil {
		return currentProblemStructure{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT problem_id,ordinal,problem_kind,parent_problem_id,subproblem_no,
		       source_number_path_json,display_label,dependency_group_id,
		       input_revision
		FROM k12_problem_structure_members
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		ORDER BY ordinal,problem_id`,
		agentName,
		submissionID,
		current.Version,
	)
	if err != nil {
		return currentProblemStructure{}, err
	}
	defer rows.Close()
	current.Members = map[string]problemStructureMember{}
	for rows.Next() {
		var member problemStructureMember
		var sourcePathJSON string
		if err := rows.Scan(
			&member.ProblemID,
			&member.Ordinal,
			&member.ProblemKind,
			&member.ParentProblemID,
			&member.SubproblemNo,
			&sourcePathJSON,
			&member.DisplayLabel,
			&member.DependencyGroupID,
			&member.InputRevision,
		); err != nil {
			return currentProblemStructure{}, err
		}
		if err := json.Unmarshal([]byte(sourcePathJSON), &member.SourceNumberPath); err != nil {
			return currentProblemStructure{}, err
		}
		current.Members[member.ProblemID] = member
	}
	if err := rows.Err(); err != nil {
		return currentProblemStructure{}, err
	}
	return current, nil
}

func insertProblemStructureVersionTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	submissionID string,
	version int,
	digest string,
	mappingState string,
	members []problemStructureMember,
	at int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO k12_problem_structure_snapshots (
			agent_name,submission_id,structure_version,structure_digest,
			mapping_state,current_disposition,created_at,updated_at
		) VALUES (?,?,?,?,?,'current',?,?)`,
		agentName,
		submissionID,
		version,
		digest,
		mappingState,
		at,
		at,
	); err != nil {
		return err
	}
	groups := map[string]struct{}{}
	for _, member := range members {
		sourcePathJSON, err := json.Marshal(member.SourceNumberPath)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_structure_members (
				agent_name,submission_id,structure_version,problem_id,ordinal,
				problem_kind,parent_problem_id,subproblem_no,
				source_number_path_json,display_label,dependency_group_id,
				input_revision
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			agentName,
			submissionID,
			version,
			member.ProblemID,
			member.Ordinal,
			member.ProblemKind,
			member.ParentProblemID,
			member.SubproblemNo,
			string(sourcePathJSON),
			member.DisplayLabel,
			member.DependencyGroupID,
			member.InputRevision,
		); err != nil {
			return err
		}
		groups[member.DependencyGroupID] = struct{}{}
	}
	groupIDs := make([]string, 0, len(groups))
	for groupID := range groups {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Strings(groupIDs)
	for _, groupID := range groupIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_dependency_groups (
				agent_name,submission_id,structure_version,dependency_group_id,
				state,state_revision,created_at,updated_at
			) VALUES (?,?,?,?,'pending',1,?,?)`,
			agentName,
			submissionID,
			version,
			groupID,
			at,
			at,
		); err != nil {
			return err
		}
	}
	return nil
}

func sameProblemStructureMember(
	a problemStructureMember,
	b problemStructureMember,
) bool {
	rawA, _ := json.Marshal(a)
	rawB, _ := json.Marshal(b)
	return string(rawA) == string(rawB)
}

func currentDurableProblemInputRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	problemID string,
) (int, error) {
	var revision int
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(revision) FROM (
			SELECT COALESCE(MAX(input_revision),0) AS revision
			FROM k12_grading_assessment_items
			WHERE agent_name=? AND problem_id=?
			UNION ALL
			SELECT COALESCE(MAX(input_revision),0)
			FROM k12_problem_skip_receipts
			WHERE agent_name=? AND problem_id=?
			UNION ALL
			SELECT COALESCE(MAX(result_input_revision),0)
			FROM k12_problem_source_action_receipts
			WHERE agent_name=? AND problem_id=?
		)`,
		agentName, problemID,
		agentName, problemID,
		agentName, problemID,
	).Scan(&revision); err != nil {
		return 0, err
	}
	return revision, nil
}

func advanceStableStructureInputRevisionsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	oldMembers map[string]problemStructureMember,
	newMembers []problemStructureMember,
) error {
	for i := range newMembers {
		oldMember, stable := oldMembers[newMembers[i].ProblemID]
		if !stable || !sameProblemStructureMember(oldMember, newMembers[i]) {
			continue
		}
		revision, err := currentDurableProblemInputRevisionTx(
			ctx, tx, agentName, newMembers[i].ProblemID,
		)
		if err != nil {
			return err
		}
		if oldMember.InputRevision > revision {
			revision = oldMember.InputRevision
		}
		if newMembers[i].InputRevision > revision {
			revision = newMembers[i].InputRevision
		}
		newMembers[i].InputRevision = revision + 1
	}
	return nil
}

func updateCurrentStructureInputRevisionsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	submissionID string,
	structureVersion int,
	members []problemStructureMember,
	at int64,
) error {
	for _, member := range members {
		if _, err := tx.ExecContext(ctx, `
			UPDATE k12_problem_structure_members
			SET input_revision=MAX(input_revision,?)
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=?`,
			member.InputRevision,
			agentName,
			submissionID,
			structureVersion,
			member.ProblemID,
		); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_structure_snapshots
		SET updated_at=MAX(updated_at,?)
		WHERE agent_name=? AND submission_id=? AND structure_version=?`,
		at, agentName, submissionID, structureVersion,
	)
	return err
}

func insertProblemStructureMappingsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	submissionID string,
	fromVersion int,
	toVersion int,
	oldMembers map[string]problemStructureMember,
	newMembers []problemStructureMember,
	at int64,
) (string, error) {
	newByID := make(map[string]problemStructureMember, len(newMembers))
	for _, member := range newMembers {
		newByID[member.ProblemID] = member
	}
	type mapping struct {
		oldID string
		newID string
		kind  string
	}
	mappings := make([]mapping, 0, len(oldMembers)+len(newByID))
	mappingState := "resolved"
	for oldID, oldMember := range oldMembers {
		if newMember, exists := newByID[oldID]; exists {
			if sameProblemStructureMember(oldMember, newMember) {
				mappings = append(mappings, mapping{oldID: oldID, newID: oldID, kind: "stable"})
			} else {
				mappings = append(mappings, mapping{oldID: oldID, newID: oldID, kind: "ambiguous"})
				mappingState = "fail_closed"
			}
			delete(newByID, oldID)
		} else {
			mappings = append(mappings, mapping{oldID: oldID, kind: "superseded"})
		}
	}
	for newID := range newByID {
		mappings = append(mappings, mapping{newID: newID, kind: "new"})
	}
	sort.Slice(mappings, func(i, j int) bool {
		left := mappings[i].oldID + "\x00" + mappings[i].newID + "\x00" + mappings[i].kind
		right := mappings[j].oldID + "\x00" + mappings[j].newID + "\x00" + mappings[j].kind
		return left < right
	})
	for _, item := range mappings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_structure_mappings (
				mapping_id,agent_name,submission_id,from_structure_version,
				to_structure_version,old_problem_id,new_problem_id,mapping_kind,
				created_at
			) VALUES (?,?,?,?,?,?,?,?,?)`,
			idgen.NanoID(),
			agentName,
			submissionID,
			fromVersion,
			toVersion,
			item.oldID,
			item.newID,
			item.kind,
			at,
		); err != nil {
			return "", err
		}
	}
	return mappingState, nil
}

func supersedeProblemStructureHeadsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	submissionID string,
	at int64,
) error {
	jobScope := `SELECT grading_job_id FROM k12_homework_submissions
		WHERE agent_name=? AND submission_id=? AND grading_job_id!=''`
	if _, err := tx.ExecContext(ctx, `
		UPDATE k12_grading_assessment_items
		SET current_disposition='superseded',updated_at=?
		WHERE agent_name=? AND current_disposition='current'
		  AND job_id IN (`+jobScope+`)`,
		at,
		agentName,
		agentName,
		submissionID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_skip_receipts
		SET current_disposition='superseded',superseded_at=?,updated_at=?
		WHERE agent_name=? AND current_disposition='current'
		  AND job_id IN (`+jobScope+`)`,
		at,
		at,
		agentName,
		agentName,
		submissionID,
	); err != nil {
		return err
	}
	return nil
}

func advanceProblemStructureSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	snapshot k12.ProblemAttemptSnapshot,
	at int64,
) error {
	agentName := snapshot.Problems[0].AgentName
	submissionID := snapshot.Problems[0].SubmissionID
	members, digest, err := problemStructureFacts(snapshot)
	if err != nil {
		return err
	}
	current, err := getCurrentProblemStructureTx(ctx, tx, agentName, submissionID)
	if errors.Is(err, sql.ErrNoRows) {
		return insertProblemStructureVersionTx(
			ctx, tx, agentName, submissionID, 1, digest, "resolved", members, at,
		)
	}
	if err != nil {
		return err
	}
	if current.Digest == digest {
		return updateCurrentStructureInputRevisionsTx(
			ctx, tx, agentName, submissionID, current.Version, members, at,
		)
	}
	if strings.HasPrefix(current.Digest, "legacy:") {
		equal := len(current.Members) == len(members)
		for _, member := range members {
			stored, ok := current.Members[member.ProblemID]
			equal = equal && ok && sameProblemStructureMember(stored, member)
		}
		if equal {
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_problem_structure_snapshots
				SET structure_digest=?,updated_at=?
				WHERE agent_name=? AND submission_id=? AND structure_version=?`,
				digest, at, agentName, submissionID, current.Version,
			); err != nil {
				return err
			}
			return updateCurrentStructureInputRevisionsTx(
				ctx, tx, agentName, submissionID, current.Version, members, at,
			)
		}
	}

	nextVersion := current.Version + 1
	if err := advanceStableStructureInputRevisionsTx(
		ctx, tx, agentName, current.Members, members,
	); err != nil {
		return err
	}
	if err := supersedeProblemStructureHeadsTx(
		ctx, tx, agentName, submissionID, at,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_structure_snapshots
		SET current_disposition='superseded',updated_at=?
		WHERE agent_name=? AND submission_id=? AND structure_version=?`,
		at,
		agentName,
		submissionID,
		current.Version,
	); err != nil {
		return err
	}
	// The old head and the target are switched inside one transaction. Any
	// member/mapping failure rolls both changes back, so no observer sees a gap.
	if err := insertProblemStructureVersionTx(
		ctx, tx, agentName, submissionID, nextVersion, digest, "resolved", members, at,
	); err != nil {
		return err
	}
	mappingState, err := insertProblemStructureMappingsTx(
		ctx,
		tx,
		agentName,
		submissionID,
		current.Version,
		nextVersion,
		current.Members,
		members,
		at,
	)
	if err != nil {
		return err
	}
	if mappingState != "resolved" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE k12_problem_structure_snapshots
			SET mapping_state=?,updated_at=?
			WHERE agent_name=? AND submission_id=? AND structure_version=?`,
			mappingState, at, agentName, submissionID, nextVersion,
		); err != nil {
			return err
		}
	}
	return nil
}
