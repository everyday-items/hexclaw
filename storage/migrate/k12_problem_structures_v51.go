package migrate

// K12ProblemStructuresV51 adds the authoritative, immutable page-structure
// ledger. Problem/Attempt remains the canonical OCR fact store; these tables
// freeze which exact-set and dependency groups are current for each submission.
var K12ProblemStructuresV51 = Migration{
	Version:     51,
	Description: "BUG-20260726-031 K12 权威题目结构版本、映射与依赖组隔离",
	SQL:         K12ProblemStructuresV51DDL,
}

const K12ProblemStructuresV51DDL = `
CREATE TABLE IF NOT EXISTS k12_problem_structure_snapshots (
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    structure_digest TEXT NOT NULL CHECK(length(trim(structure_digest)) > 0),
    mapping_state TEXT NOT NULL CHECK(mapping_state IN ('resolved','fail_closed')),
    current_disposition TEXT NOT NULL
        CHECK(current_disposition IN ('current','superseded')),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    PRIMARY KEY(agent_name,submission_id,structure_version),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_problem_structure_current
    ON k12_problem_structure_snapshots(agent_name,submission_id)
    WHERE current_disposition='current';

CREATE TABLE IF NOT EXISTS k12_problem_structure_members (
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    problem_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    problem_kind TEXT NOT NULL
        CHECK(problem_kind IN ('standalone','compound_parent','subproblem')),
    parent_problem_id TEXT NOT NULL DEFAULT '',
    subproblem_no TEXT NOT NULL DEFAULT '',
    source_number_path_json TEXT NOT NULL DEFAULT '[]'
        CHECK(json_valid(source_number_path_json)),
    display_label TEXT NOT NULL DEFAULT '',
    dependency_group_id TEXT NOT NULL CHECK(length(trim(dependency_group_id)) > 0),
    input_revision INTEGER NOT NULL CHECK(input_revision >= 1),
    PRIMARY KEY(agent_name,submission_id,structure_version,problem_id),
    UNIQUE(agent_name,submission_id,structure_version,ordinal),
    FOREIGN KEY(agent_name,submission_id,structure_version)
        REFERENCES k12_problem_structure_snapshots(
            agent_name,submission_id,structure_version
        ) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,problem_id)
        REFERENCES k12_problems(agent_name,problem_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_k12_problem_structure_members_group
    ON k12_problem_structure_members(
        agent_name,submission_id,structure_version,dependency_group_id,ordinal
    );

CREATE TABLE IF NOT EXISTS k12_problem_structure_mappings (
    mapping_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    from_structure_version INTEGER NOT NULL CHECK(from_structure_version >= 1),
    to_structure_version INTEGER NOT NULL CHECK(to_structure_version > from_structure_version),
    old_problem_id TEXT NOT NULL DEFAULT '',
    new_problem_id TEXT NOT NULL DEFAULT '',
    mapping_kind TEXT NOT NULL
        CHECK(mapping_kind IN ('stable','new','superseded','ambiguous')),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(
        agent_name,submission_id,from_structure_version,to_structure_version,
        old_problem_id,new_problem_id,mapping_kind
    ),
    FOREIGN KEY(agent_name,submission_id,from_structure_version)
        REFERENCES k12_problem_structure_snapshots(
            agent_name,submission_id,structure_version
        ) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,submission_id,to_structure_version)
        REFERENCES k12_problem_structure_snapshots(
            agent_name,submission_id,structure_version
        ) ON DELETE CASCADE,
    CHECK(old_problem_id != '' OR new_problem_id != ''),
    CHECK(
        (mapping_kind='stable' AND old_problem_id!='' AND old_problem_id=new_problem_id)
        OR (mapping_kind='new' AND old_problem_id='' AND new_problem_id!='')
        OR (mapping_kind='superseded' AND old_problem_id!='' AND new_problem_id='')
        OR (mapping_kind='ambiguous' AND old_problem_id!='' AND new_problem_id!='')
    )
);

CREATE TABLE IF NOT EXISTS k12_problem_dependency_groups (
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    dependency_group_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN
        ('pending','ready','blocked','processing','completed','failed')),
    state_revision INTEGER NOT NULL DEFAULT 1 CHECK(state_revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    PRIMARY KEY(agent_name,submission_id,structure_version,dependency_group_id),
    FOREIGN KEY(agent_name,submission_id,structure_version)
        REFERENCES k12_problem_structure_snapshots(
            agent_name,submission_id,structure_version
        ) ON DELETE CASCADE
);

INSERT OR IGNORE INTO k12_problem_structure_snapshots (
    agent_name,submission_id,structure_version,structure_digest,mapping_state,
    current_disposition,created_at,updated_at
)
SELECT agent_name,submission_id,1,
       'legacy:' || agent_name || ':' || submission_id,
       'resolved','current',
       MAX(1,MIN(created_at)),MAX(1,MAX(updated_at))
FROM k12_problems
GROUP BY agent_name,submission_id;

INSERT OR IGNORE INTO k12_problem_structure_members (
    agent_name,submission_id,structure_version,problem_id,ordinal,problem_kind,
    parent_problem_id,subproblem_no,source_number_path_json,display_label,
    dependency_group_id,input_revision
)
SELECT p.agent_name,p.submission_id,1,p.problem_id,p.ordinal,p.problem_kind,
       COALESCE(p.parent_problem_id,''),p.subproblem_no,p.source_number_path_json,
       p.display_label,
       CASE
         WHEN p.problem_kind='subproblem' THEN 'parent:' || p.parent_problem_id
         ELSE 'problem:' || p.problem_id
       END,
       MAX(1,COALESCE(a.confirmed_version,1))
FROM k12_problems p
LEFT JOIN k12_attempts a
  ON a.agent_name=p.agent_name
 AND a.submission_id=p.submission_id
 AND a.problem_id=p.problem_id
GROUP BY p.agent_name,p.submission_id,p.problem_id;

INSERT OR IGNORE INTO k12_problem_dependency_groups (
    agent_name,submission_id,structure_version,dependency_group_id,state,
    state_revision,created_at,updated_at
)
SELECT m.agent_name,m.submission_id,m.structure_version,m.dependency_group_id,
       'pending',1,s.created_at,s.updated_at
FROM k12_problem_structure_members m
JOIN k12_problem_structure_snapshots s
  ON s.agent_name=m.agent_name
 AND s.submission_id=m.submission_id
 AND s.structure_version=m.structure_version
GROUP BY m.agent_name,m.submission_id,m.structure_version,m.dependency_group_id;`
