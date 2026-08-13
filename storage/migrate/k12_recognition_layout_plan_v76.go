package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// K12RecognitionLayoutPlanV76 引入持久化的 DD-036 V2 识别计划，
// 同时保持 V1 物理单元契约逐字节不变。SQLite 将物理单元枚举嵌入表的
// CHECK 约束，因此需在同一专用连接上重建两张固化该单元的账本，
// 并在同一事务中连同迁移版本记录一起提交。
var K12RecognitionLayoutPlanV76 = Migration{
	Version:     76,
	Description: "DD-036 K12 durable V2 recognition layout plan and bounded repair authorization",
	AtomicFunc:  migrateK12RecognitionLayoutPlanV76,
}

const k12RecognitionLayoutPlanV76PhysicalTableDDL = `
CREATE TABLE k12_model_physical_invocations_v76 (
    physical_invocation_id TEXT PRIMARY KEY,
    parent_invocation_id TEXT NOT NULL,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    stage TEXT NOT NULL CHECK(stage = 'recognizing'),
    physical_unit TEXT NOT NULL CHECK(
        physical_unit IN
            ('whole_page','segment_1','segment_2','segment_3','segment_4','segment_5','printed_inventory')
        OR (
            physical_unit GLOB 'layout_batch_[0-9][0-9][0-9][0-9]'
            AND substr(physical_unit,14,4) BETWEEN '0001' AND '9999'
        )
        OR (
            physical_unit GLOB 'layout_repair_[0-9][0-9][0-9][0-9]'
            AND substr(physical_unit,15,4) BETWEEN '0001' AND '9999'
        )
    ),
    request_digest TEXT NOT NULL CHECK(request_digest != ''),
    route_snapshot_json TEXT NOT NULL CHECK(route_snapshot_json != ''),
    request_policy_snapshot_json TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN
        ('prepared','sent','succeeded','failed','outcome_unknown','reconciled')),
    attempt INTEGER NOT NULL CHECK(attempt = 1),
    result_digest TEXT NOT NULL DEFAULT '',
    result_content TEXT,
    external_request_id TEXT NOT NULL DEFAULT '',
    failure_kind TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    recognition_plan_version TEXT NOT NULL DEFAULT 'v1'
        CHECK(recognition_plan_version IN ('v1','v2')),
    plan_digest TEXT NOT NULL DEFAULT '',
    candidate_exact_set_digest TEXT NOT NULL DEFAULT '',
    UNIQUE(parent_invocation_id,physical_unit),
    FOREIGN KEY(parent_invocation_id,agent_name,job_id,stage)
        REFERENCES k12_model_invocations(invocation_id,agent_name,job_id,stage)
        ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
    FOREIGN KEY(job_id) REFERENCES k12_grading_jobs(record_id) ON DELETE CASCADE,
	-- V6 归档恢复明确成功的回执时，可能会有意脱敏原始 Provider 内容。
	-- 非空摘要与题源绑定触发器仍构成持久证据。
	CHECK(status != 'succeeded' OR result_digest != ''),
    CHECK(status = 'succeeded' OR result_content IS NULL),
    CHECK(status NOT IN ('failed','outcome_unknown') OR failure_kind != ''),
    CHECK(
        (
            recognition_plan_version='v1'
            AND physical_unit IN
                ('whole_page','segment_1','segment_2','segment_3','segment_4','segment_5','printed_inventory')
            AND plan_digest=''
            AND candidate_exact_set_digest=''
        )
        OR (
            recognition_plan_version='v2'
            AND plan_digest!=''
            AND (
                (physical_unit='whole_page' AND candidate_exact_set_digest='')
                OR (
                    candidate_exact_set_digest!=''
                    AND (
                        (
                            physical_unit GLOB 'layout_batch_[0-9][0-9][0-9][0-9]'
                            AND substr(physical_unit,14,4) BETWEEN '0001' AND '9999'
                        )
                        OR (
                            physical_unit GLOB 'layout_repair_[0-9][0-9][0-9][0-9]'
                            AND substr(physical_unit,15,4) BETWEEN '0001' AND '9999'
                        )
                    )
                )
            )
        )
    )
);
CREATE UNIQUE INDEX idx_k12_model_physical_invocation_parent_identity_v76
    ON k12_model_physical_invocations_v76(
        physical_invocation_id,parent_invocation_id
    );`

const k12RecognitionLayoutPlanV76PhysicalResultTableDDL = `
CREATE TABLE k12_problem_source_recognition_physical_results_v76 (
    work_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    parent_invocation_id TEXT NOT NULL CHECK(length(trim(parent_invocation_id)) > 0),
    physical_invocation_id TEXT NOT NULL CHECK(length(trim(physical_invocation_id)) > 0),
    physical_unit TEXT NOT NULL CHECK(
        physical_unit IN
            ('whole_page','segment_1','segment_2','segment_3','segment_4','segment_5','printed_inventory')
        OR (
            physical_unit GLOB 'layout_batch_[0-9][0-9][0-9][0-9]'
            AND substr(physical_unit,14,4) BETWEEN '0001' AND '9999'
        )
        OR (
            physical_unit GLOB 'layout_repair_[0-9][0-9][0-9][0-9]'
            AND substr(physical_unit,15,4) BETWEEN '0001' AND '9999'
        )
    ),
    result_digest TEXT NOT NULL CHECK(length(trim(result_digest)) > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    recognition_plan_version TEXT NOT NULL DEFAULT 'v1'
        CHECK(recognition_plan_version IN ('v1','v2')),
    plan_digest TEXT NOT NULL DEFAULT '',
    candidate_exact_set_digest TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(work_id,physical_invocation_id),
    UNIQUE(work_id,ordinal),
    FOREIGN KEY(work_id,parent_invocation_id)
        REFERENCES k12_problem_source_recognition_results(work_id,parent_invocation_id)
        ON DELETE CASCADE,
    FOREIGN KEY(physical_invocation_id,parent_invocation_id)
        REFERENCES k12_model_physical_invocations_v76(
            physical_invocation_id,parent_invocation_id
        ) ON DELETE RESTRICT,
    CHECK(
        (
            recognition_plan_version='v1'
            AND physical_unit IN
                ('whole_page','segment_1','segment_2','segment_3','segment_4','segment_5','printed_inventory')
            AND plan_digest=''
            AND candidate_exact_set_digest=''
        )
        OR (
            recognition_plan_version='v2'
            AND plan_digest!=''
            AND (
                (physical_unit='whole_page' AND candidate_exact_set_digest='')
                OR (
                    candidate_exact_set_digest!=''
                    AND (
                        (
                            physical_unit GLOB 'layout_batch_[0-9][0-9][0-9][0-9]'
                            AND substr(physical_unit,14,4) BETWEEN '0001' AND '9999'
                        )
                        OR (
                            physical_unit GLOB 'layout_repair_[0-9][0-9][0-9][0-9]'
                            AND substr(physical_unit,15,4) BETWEEN '0001' AND '9999'
                        )
                    )
                )
            )
        )
    )
);`

const k12RecognitionLayoutPlanV76PhysicalPostDDL = `
CREATE INDEX IF NOT EXISTS idx_k12_model_physical_invocations_job
    ON k12_model_physical_invocations(
        agent_name,job_id,parent_invocation_id,physical_unit
    );
CREATE INDEX IF NOT EXISTS idx_k12_model_physical_invocations_status
    ON k12_model_physical_invocations(status,updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_model_physical_invocation_parent_identity
    ON k12_model_physical_invocations(
        physical_invocation_id,parent_invocation_id
    );
DROP INDEX IF EXISTS idx_k12_model_physical_invocation_parent_identity_v76;

CREATE TRIGGER IF NOT EXISTS k12_model_physical_invocation_identity_immutable
BEFORE UPDATE OF
    physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
    physical_unit,request_digest,route_snapshot_json,
    request_policy_snapshot_json,attempt,created_at,
    recognition_plan_version,plan_digest,candidate_exact_set_digest
ON k12_model_physical_invocations
BEGIN
    SELECT RAISE(ABORT, 'model physical invocation identity is immutable');
END;`

const k12RecognitionLayoutPlanV76PhysicalResultPostDDL = `
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_recognition_physical_parent
    ON k12_problem_source_recognition_physical_results(
        parent_invocation_id,physical_invocation_id,work_id
    );
CREATE TRIGGER IF NOT EXISTS k12_problem_source_recognition_physical_result_immutable
BEFORE UPDATE ON k12_problem_source_recognition_physical_results
BEGIN
    SELECT RAISE(ABORT, 'problem source recognition physical result is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_problem_source_recognition_physical_result_binding_guard
BEFORE INSERT ON k12_problem_source_recognition_physical_results
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_model_physical_invocations child
    WHERE child.physical_invocation_id=NEW.physical_invocation_id
      AND child.parent_invocation_id=NEW.parent_invocation_id
      AND child.physical_unit=NEW.physical_unit
      AND child.result_digest=NEW.result_digest
      AND child.recognition_plan_version=NEW.recognition_plan_version
      AND child.plan_digest=NEW.plan_digest
      AND child.candidate_exact_set_digest=NEW.candidate_exact_set_digest
      AND child.status='succeeded'
)
BEGIN
    SELECT RAISE(ABORT, 'recognition physical result is detached from child evidence');
END;`

const k12RecognitionLayoutPlanV76AuthorizationDDL = `
CREATE TABLE IF NOT EXISTS k12_recognition_layout_plans (
    plan_id TEXT PRIMARY KEY CHECK(length(trim(plan_id)) > 0),
    parent_invocation_id TEXT NOT NULL UNIQUE,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    stage TEXT NOT NULL DEFAULT 'recognizing' CHECK(stage='recognizing'),
    manifest_physical_invocation_id TEXT NOT NULL UNIQUE,
    page_digest TEXT NOT NULL CHECK(length(trim(page_digest)) > 0),
    header_digest TEXT NOT NULL CHECK(length(trim(header_digest)) > 0),
    manifest_result_digest TEXT NOT NULL DEFAULT '',
    authorized_plan_digest TEXT NOT NULL DEFAULT '',
    candidate_exact_set_digest TEXT NOT NULL DEFAULT '',
    layout_header_json TEXT NOT NULL
        CHECK(json_valid(layout_header_json) AND json_type(layout_header_json)='object'),
    authorized_plan_json TEXT NOT NULL DEFAULT ''
        CHECK(authorized_plan_json='' OR (
            json_valid(authorized_plan_json) AND json_type(authorized_plan_json)='object'
        )),
    stage_started_at INTEGER NOT NULL CHECK(stage_started_at > 0),
    stage_deadline_at INTEGER NOT NULL DEFAULT 0
        CHECK(stage_deadline_at=0 OR stage_deadline_at > stage_started_at),
    selected_bucket_max_problems INTEGER NOT NULL DEFAULT 0
        CHECK(selected_bucket_max_problems IN (0,1,8,16,32)),
    effective_concurrency INTEGER NOT NULL CHECK(effective_concurrency BETWEEN 1 AND 2),
    status TEXT NOT NULL CHECK(status IN (
        'prepared_manifest','manifest_sent','manifest_succeeded','authorized',
        'running','succeeded','failed','outcome_unknown','cancelled'
    )),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(plan_id,parent_invocation_id),
    FOREIGN KEY(parent_invocation_id,agent_name,job_id,stage)
        REFERENCES k12_model_invocations(invocation_id,agent_name,job_id,stage)
        ON DELETE CASCADE,
    FOREIGN KEY(manifest_physical_invocation_id,parent_invocation_id)
        REFERENCES k12_model_physical_invocations(
            physical_invocation_id,parent_invocation_id
        ) ON DELETE CASCADE,
    CHECK(
        (authorized_plan_digest='' AND candidate_exact_set_digest='' AND authorized_plan_json='')
        OR
        (authorized_plan_digest!='' AND candidate_exact_set_digest!='' AND authorized_plan_json!='')
    ),
    CHECK(status NOT IN ('manifest_succeeded','authorized','running','succeeded')
        OR manifest_result_digest!=''),
    CHECK(status NOT IN ('authorized','running','succeeded')
        OR authorized_plan_digest!=''),
    CHECK(
        (selected_bucket_max_problems=0 AND stage_deadline_at=0)
        OR
        (selected_bucket_max_problems IN (1,8,16,32)
            AND stage_deadline_at > stage_started_at)
    ),
    CHECK(status NOT IN ('authorized','running','succeeded')
        OR selected_bucket_max_problems IN (1,8,16,32))
);
CREATE INDEX IF NOT EXISTS idx_k12_recognition_layout_plans_owner
    ON k12_recognition_layout_plans(agent_name,job_id,parent_invocation_id,status);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_candidates (
    plan_id TEXT NOT NULL,
    candidate_id TEXT NOT NULL CHECK(length(trim(candidate_id)) > 0),
    ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 1 AND 32),
    bbox_x INTEGER NOT NULL CHECK(bbox_x >= 0),
    bbox_y INTEGER NOT NULL CHECK(bbox_y >= 0),
    bbox_width INTEGER NOT NULL CHECK(bbox_width > 0),
    bbox_height INTEGER NOT NULL CHECK(bbox_height > 0),
    crop_digest TEXT NOT NULL CHECK(length(trim(crop_digest)) > 0),
    candidate_json TEXT NOT NULL
        CHECK(json_valid(candidate_json) AND json_type(candidate_json)='object'),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(plan_id,candidate_id),
    UNIQUE(plan_id,ordinal),
    FOREIGN KEY(plan_id) REFERENCES k12_recognition_layout_plans(plan_id)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_batches (
    plan_id TEXT NOT NULL,
    batch_id TEXT NOT NULL CHECK(length(trim(batch_id)) > 0),
    ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 1 AND 9999),
    physical_unit TEXT NOT NULL CHECK(
        physical_unit GLOB 'layout_batch_[0-9][0-9][0-9][0-9]'
        AND substr(physical_unit,14,4) BETWEEN '0001' AND '9999'
        AND CAST(substr(physical_unit,14,4) AS INTEGER)=ordinal
    ),
    member_count INTEGER NOT NULL CHECK(member_count BETWEEN 1 AND 4),
    batch_digest TEXT NOT NULL CHECK(length(trim(batch_digest)) > 0),
    input_digest TEXT NOT NULL CHECK(length(trim(input_digest)) > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(plan_id,batch_id),
    UNIQUE(plan_id,ordinal),
    UNIQUE(plan_id,physical_unit),
    FOREIGN KEY(plan_id) REFERENCES k12_recognition_layout_plans(plan_id)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_batch_members (
    plan_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    slot INTEGER NOT NULL CHECK(slot BETWEEN 0 AND 3),
    candidate_id TEXT NOT NULL,
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(plan_id,batch_id,slot),
    UNIQUE(plan_id,candidate_id),
    FOREIGN KEY(plan_id,batch_id)
        REFERENCES k12_recognition_layout_batches(plan_id,batch_id)
        ON DELETE CASCADE,
    FOREIGN KEY(plan_id,candidate_id)
        REFERENCES k12_recognition_layout_candidates(plan_id,candidate_id)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_batch_settlements (
    plan_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    parent_invocation_id TEXT NOT NULL,
    source_physical_invocation_id TEXT NOT NULL,
    source_physical_unit TEXT NOT NULL CHECK(
        source_physical_unit GLOB 'layout_batch_[0-9][0-9][0-9][0-9]'
        AND substr(source_physical_unit,14,4) BETWEEN '0001' AND '9999'
    ),
    source_physical_result_digest TEXT NOT NULL
        CHECK(length(trim(source_physical_result_digest)) > 0),
    classification TEXT NOT NULL CHECK(classification IN (
        'classified','terminal_ambiguous'
    )),
    ambiguity_kind TEXT NOT NULL DEFAULT '' CHECK(ambiguity_kind IN (
        '','extra_candidate','duplicate_candidate','source_conflict',
        'unattributable'
    )),
    settlement_digest TEXT NOT NULL CHECK(length(trim(settlement_digest)) > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(plan_id,batch_id),
    UNIQUE(source_physical_invocation_id),
    FOREIGN KEY(plan_id,batch_id)
        REFERENCES k12_recognition_layout_batches(plan_id,batch_id)
        ON DELETE CASCADE,
    FOREIGN KEY(plan_id,parent_invocation_id)
        REFERENCES k12_recognition_layout_plans(plan_id,parent_invocation_id)
        ON DELETE CASCADE,
    FOREIGN KEY(source_physical_invocation_id,parent_invocation_id)
        REFERENCES k12_model_physical_invocations(
            physical_invocation_id,parent_invocation_id
        ) ON DELETE CASCADE,
    CHECK(
        (classification='classified' AND ambiguity_kind='')
        OR
        (classification='terminal_ambiguous' AND ambiguity_kind!='')
    )
);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_candidate_results (
    plan_id TEXT NOT NULL,
    candidate_id TEXT NOT NULL,
    parent_invocation_id TEXT NOT NULL,
    source_physical_invocation_id TEXT NOT NULL,
    source_physical_result_digest TEXT NOT NULL
        CHECK(length(trim(source_physical_result_digest)) > 0),
    result_kind TEXT NOT NULL CHECK(result_kind IN ('question','non_question')),
    result_digest TEXT NOT NULL CHECK(length(trim(result_digest)) > 0),
    result_json TEXT NOT NULL
        CHECK(json_valid(result_json) AND json_type(result_json)='object'),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(plan_id,candidate_id),
    FOREIGN KEY(plan_id,candidate_id)
        REFERENCES k12_recognition_layout_candidates(plan_id,candidate_id)
        ON DELETE CASCADE,
    FOREIGN KEY(plan_id,parent_invocation_id)
        REFERENCES k12_recognition_layout_plans(plan_id,parent_invocation_id)
        ON DELETE CASCADE,
    FOREIGN KEY(source_physical_invocation_id,parent_invocation_id)
        REFERENCES k12_model_physical_invocations(
            physical_invocation_id,parent_invocation_id
        ) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_repair_authorizations (
    plan_id TEXT NOT NULL,
    repair_authorization_id TEXT NOT NULL UNIQUE
        CHECK(length(trim(repair_authorization_id)) > 0),
    repair_physical_unit TEXT NOT NULL CHECK(
        repair_physical_unit GLOB 'layout_repair_[0-9][0-9][0-9][0-9]'
        AND substr(repair_physical_unit,15,4) BETWEEN '0001' AND '9999'
    ),
    candidate_id TEXT NOT NULL,
    source_batch_id TEXT NOT NULL,
    source_batch_physical_invocation_id TEXT NOT NULL,
    source_batch_result_digest TEXT NOT NULL
        CHECK(length(trim(source_batch_result_digest)) > 0),
    repair_round INTEGER NOT NULL CHECK(repair_round=1),
    authorization_digest TEXT NOT NULL CHECK(length(trim(authorization_digest)) > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(plan_id,candidate_id),
    UNIQUE(plan_id,repair_physical_unit),
    UNIQUE(
        plan_id,candidate_id,repair_authorization_id,
        repair_physical_unit,authorization_digest
    ),
    FOREIGN KEY(plan_id,candidate_id)
        REFERENCES k12_recognition_layout_candidates(plan_id,candidate_id)
        ON DELETE CASCADE,
    FOREIGN KEY(plan_id,source_batch_id)
        REFERENCES k12_recognition_layout_batches(plan_id,batch_id)
        ON DELETE CASCADE,
    FOREIGN KEY(source_batch_physical_invocation_id)
        REFERENCES k12_model_physical_invocations(physical_invocation_id)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_repair_settlements (
    plan_id TEXT NOT NULL,
    repair_authorization_id TEXT NOT NULL,
    authorization_digest TEXT NOT NULL
        CHECK(length(trim(authorization_digest)) > 0),
    candidate_id TEXT NOT NULL,
    parent_invocation_id TEXT NOT NULL,
    source_physical_invocation_id TEXT NOT NULL,
    source_physical_unit TEXT NOT NULL CHECK(
        source_physical_unit GLOB 'layout_repair_[0-9][0-9][0-9][0-9]'
        AND substr(source_physical_unit,15,4) BETWEEN '0001' AND '9999'
    ),
    source_physical_result_digest TEXT NOT NULL
        CHECK(length(trim(source_physical_result_digest)) > 0),
    classification TEXT NOT NULL CHECK(classification IN ('valid','invalid')),
    result_kind TEXT NOT NULL DEFAULT '' CHECK(result_kind IN (
        '','question','non_question'
    )),
    result_digest TEXT NOT NULL DEFAULT '',
    settlement_digest TEXT NOT NULL CHECK(length(trim(settlement_digest)) > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(plan_id,candidate_id),
    UNIQUE(repair_authorization_id),
    UNIQUE(source_physical_invocation_id),
    FOREIGN KEY(
        plan_id,candidate_id,repair_authorization_id,
        source_physical_unit,authorization_digest
    ) REFERENCES k12_recognition_layout_repair_authorizations(
        plan_id,candidate_id,repair_authorization_id,
        repair_physical_unit,authorization_digest
    ) ON DELETE CASCADE,
    FOREIGN KEY(plan_id,parent_invocation_id)
        REFERENCES k12_recognition_layout_plans(plan_id,parent_invocation_id)
        ON DELETE CASCADE,
    FOREIGN KEY(source_physical_invocation_id,parent_invocation_id)
        REFERENCES k12_model_physical_invocations(
            physical_invocation_id,parent_invocation_id
        ) ON DELETE CASCADE,
    CHECK(
        (classification='valid' AND result_kind!='' AND result_digest!='')
        OR
        (classification='invalid' AND result_kind='' AND result_digest='')
    )
);

CREATE TABLE IF NOT EXISTS k12_recognition_layout_finalizations (
    plan_id TEXT PRIMARY KEY,
    parent_invocation_id TEXT NOT NULL UNIQUE,
    authorized_plan_digest TEXT NOT NULL
        CHECK(length(trim(authorized_plan_digest)) > 0),
    candidate_exact_set_digest TEXT NOT NULL
        CHECK(length(trim(candidate_exact_set_digest)) > 0),
    candidate_results_exact_set_digest TEXT NOT NULL
        CHECK(length(trim(candidate_results_exact_set_digest)) > 0),
    physical_results_exact_set_digest TEXT NOT NULL
        CHECK(length(trim(physical_results_exact_set_digest)) > 0),
    candidate_result_count INTEGER NOT NULL CHECK(candidate_result_count BETWEEN 1 AND 32),
    physical_result_count INTEGER NOT NULL CHECK(physical_result_count BETWEEN 2 AND 65),
    finalization_json TEXT NOT NULL
        CHECK(json_valid(finalization_json) AND json_type(finalization_json)='object'),
    finalization_digest TEXT NOT NULL
        CHECK(length(trim(finalization_digest)) > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    FOREIGN KEY(plan_id,parent_invocation_id)
        REFERENCES k12_recognition_layout_plans(plan_id,parent_invocation_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_k12_recognition_layout_candidates_order
    ON k12_recognition_layout_candidates(plan_id,ordinal,candidate_id);
CREATE INDEX IF NOT EXISTS idx_k12_recognition_layout_batches_order
    ON k12_recognition_layout_batches(plan_id,ordinal,physical_unit);
CREATE INDEX IF NOT EXISTS idx_k12_recognition_layout_batch_settlement_source
    ON k12_recognition_layout_batch_settlements(
        source_physical_invocation_id,plan_id,batch_id
    );
CREATE INDEX IF NOT EXISTS idx_k12_recognition_layout_candidate_results_source
    ON k12_recognition_layout_candidate_results(
        source_physical_invocation_id,plan_id,candidate_id
    );
CREATE INDEX IF NOT EXISTS idx_k12_recognition_layout_repairs_source
    ON k12_recognition_layout_repair_authorizations(
        source_batch_physical_invocation_id,plan_id,candidate_id
    );
CREATE INDEX IF NOT EXISTS idx_k12_recognition_layout_repair_settlement_source
    ON k12_recognition_layout_repair_settlements(
        source_physical_invocation_id,plan_id,candidate_id
    );
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_recognition_layout_finalization_digest
    ON k12_recognition_layout_finalizations(finalization_digest);

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_plan_manifest_insert_guard
BEFORE INSERT ON k12_recognition_layout_plans
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_model_physical_invocations child
    WHERE child.physical_invocation_id=NEW.manifest_physical_invocation_id
      AND child.parent_invocation_id=NEW.parent_invocation_id
      AND child.agent_name=NEW.agent_name
      AND child.job_id=NEW.job_id
      AND child.physical_unit='whole_page'
      AND child.recognition_plan_version='v2'
      AND child.plan_digest=NEW.header_digest
      AND (
          NEW.manifest_result_digest=''
          OR (
              child.status='succeeded'
              AND child.result_digest=NEW.manifest_result_digest
          )
      )
)
BEGIN
    SELECT RAISE(ABORT, 'layout plan is detached from manifest child');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_plan_identity_immutable
BEFORE UPDATE OF
    plan_id,parent_invocation_id,agent_name,job_id,stage,
    manifest_physical_invocation_id,page_digest,header_digest,
    layout_header_json,stage_started_at,
    effective_concurrency,created_at
ON k12_recognition_layout_plans
BEGIN
    SELECT RAISE(ABORT, 'recognition layout plan identity is immutable');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_plan_deadline_once
BEFORE UPDATE OF selected_bucket_max_problems,stage_deadline_at
ON k12_recognition_layout_plans
WHEN NOT (
    (
        NEW.selected_bucket_max_problems=OLD.selected_bucket_max_problems
        AND NEW.stage_deadline_at=OLD.stage_deadline_at
    )
    OR
    (
        OLD.status='manifest_succeeded'
        AND NEW.status='authorized'
        AND OLD.selected_bucket_max_problems=0
        AND OLD.stage_deadline_at=0
        AND NEW.selected_bucket_max_problems IN (1,8,16,32)
        AND NEW.stage_deadline_at > OLD.stage_started_at
    )
)
BEGIN
    SELECT RAISE(ABORT, 'recognition layout bucket deadline may be selected only once');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_plan_authorization_once
BEFORE UPDATE OF
    manifest_result_digest,authorized_plan_digest,
    candidate_exact_set_digest,authorized_plan_json
ON k12_recognition_layout_plans
WHEN
    (OLD.manifest_result_digest!='' AND NEW.manifest_result_digest!=OLD.manifest_result_digest)
    OR (OLD.authorized_plan_digest!='' AND NEW.authorized_plan_digest!=OLD.authorized_plan_digest)
    OR (OLD.candidate_exact_set_digest!='' AND NEW.candidate_exact_set_digest!=OLD.candidate_exact_set_digest)
    OR (OLD.authorized_plan_json!='' AND NEW.authorized_plan_json!=OLD.authorized_plan_json)
BEGIN
    SELECT RAISE(ABORT, 'recognition layout authorization is immutable once set');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_plan_status_guard
BEFORE UPDATE OF status ON k12_recognition_layout_plans
WHEN NEW.status!=OLD.status AND NOT (
    (OLD.status='prepared_manifest' AND NEW.status IN ('manifest_sent','failed','cancelled'))
    OR (OLD.status='manifest_sent' AND NEW.status IN (
        'manifest_succeeded','failed','outcome_unknown','cancelled'
    ))
    OR (OLD.status='manifest_succeeded' AND NEW.status IN ('authorized','failed','cancelled'))
    OR (OLD.status='authorized' AND NEW.status IN ('running','failed','cancelled'))
    OR (OLD.status='running' AND NEW.status IN (
        'succeeded','failed','outcome_unknown','cancelled'
    ))
)
BEGIN
    SELECT RAISE(ABORT, 'invalid recognition layout plan status transition');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_plan_updated_at_guard
BEFORE UPDATE ON k12_recognition_layout_plans
WHEN NEW.updated_at < OLD.updated_at
BEGIN
    SELECT RAISE(ABORT, 'recognition layout plan updated_at regressed');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_batch_member_guard
BEFORE INSERT ON k12_recognition_layout_batch_members
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_recognition_layout_batches batch
    WHERE batch.plan_id=NEW.plan_id
      AND batch.batch_id=NEW.batch_id
      AND NEW.slot < batch.member_count
)
BEGIN
    SELECT RAISE(ABORT, 'layout batch member exceeds authorized member count');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_batch_settlement_source_guard
BEFORE INSERT ON k12_recognition_layout_batch_settlements
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_recognition_layout_plans plan
    JOIN k12_recognition_layout_batches batch
      ON batch.plan_id=plan.plan_id AND batch.batch_id=NEW.batch_id
    JOIN k12_model_physical_invocations child
      ON child.physical_invocation_id=NEW.source_physical_invocation_id
     AND child.parent_invocation_id=plan.parent_invocation_id
    WHERE plan.plan_id=NEW.plan_id
      AND plan.parent_invocation_id=NEW.parent_invocation_id
      AND plan.status IN ('authorized','running')
      AND batch.physical_unit=NEW.source_physical_unit
      AND child.agent_name=plan.agent_name
      AND child.job_id=plan.job_id
      AND child.stage=plan.stage
      AND child.physical_unit=batch.physical_unit
      AND child.recognition_plan_version='v2'
      AND child.plan_digest=plan.authorized_plan_digest
      AND child.status='succeeded'
      AND child.result_digest=NEW.source_physical_result_digest
)
BEGIN
    SELECT RAISE(ABORT, 'batch settlement is detached from succeeded source evidence');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_candidate_result_source_guard
BEFORE INSERT ON k12_recognition_layout_candidate_results
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_model_physical_invocations child
    WHERE child.physical_invocation_id=NEW.source_physical_invocation_id
      AND child.parent_invocation_id=NEW.parent_invocation_id
      AND child.recognition_plan_version='v2'
      AND child.status='succeeded'
      AND child.result_digest=NEW.source_physical_result_digest
      AND (
          EXISTS (
              SELECT 1
              FROM k12_recognition_layout_batches batch
              JOIN k12_recognition_layout_batch_members member
                ON member.plan_id=batch.plan_id AND member.batch_id=batch.batch_id
              WHERE batch.plan_id=NEW.plan_id
                AND member.candidate_id=NEW.candidate_id
                AND batch.physical_unit=child.physical_unit
          )
          OR EXISTS (
              SELECT 1
              FROM k12_recognition_layout_repair_authorizations repair
              JOIN k12_recognition_layout_repair_settlements settlement
                ON settlement.plan_id=repair.plan_id
               AND settlement.candidate_id=repair.candidate_id
               AND settlement.repair_authorization_id=
                    repair.repair_authorization_id
              WHERE repair.plan_id=NEW.plan_id
                AND repair.candidate_id=NEW.candidate_id
                AND repair.repair_physical_unit=child.physical_unit
                AND settlement.parent_invocation_id=NEW.parent_invocation_id
                AND settlement.source_physical_invocation_id=
                    NEW.source_physical_invocation_id
                AND settlement.source_physical_result_digest=
                    NEW.source_physical_result_digest
                AND settlement.classification='valid'
                AND settlement.result_kind=NEW.result_kind
                AND settlement.result_digest=NEW.result_digest
          )
      )
)
BEGIN
    SELECT RAISE(ABORT, 'candidate result is detached from authorized physical evidence');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_repair_source_guard
BEFORE INSERT ON k12_recognition_layout_repair_authorizations
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_recognition_layout_plans plan
    JOIN k12_recognition_layout_batches batch
      ON batch.plan_id=plan.plan_id AND batch.batch_id=NEW.source_batch_id
    JOIN k12_recognition_layout_batch_members member
      ON member.plan_id=batch.plan_id AND member.batch_id=batch.batch_id
    JOIN k12_model_physical_invocations child
      ON child.physical_invocation_id=NEW.source_batch_physical_invocation_id
     AND child.parent_invocation_id=plan.parent_invocation_id
    WHERE plan.plan_id=NEW.plan_id
      AND member.candidate_id=NEW.candidate_id
      AND child.physical_unit=batch.physical_unit
      AND child.recognition_plan_version='v2'
      AND child.status='succeeded'
      AND child.result_digest=NEW.source_batch_result_digest
)
BEGIN
    SELECT RAISE(ABORT, 'repair authorization is detached from source batch result');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_repair_settlement_source_guard
BEFORE INSERT ON k12_recognition_layout_repair_settlements
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_recognition_layout_plans plan
    JOIN k12_recognition_layout_repair_authorizations repair
      ON repair.plan_id=plan.plan_id
     AND repair.candidate_id=NEW.candidate_id
     AND repair.repair_authorization_id=NEW.repair_authorization_id
     AND repair.repair_physical_unit=NEW.source_physical_unit
     AND repair.authorization_digest=NEW.authorization_digest
    JOIN k12_model_physical_invocations child
      ON child.physical_invocation_id=NEW.source_physical_invocation_id
     AND child.parent_invocation_id=plan.parent_invocation_id
    WHERE plan.plan_id=NEW.plan_id
      AND plan.parent_invocation_id=NEW.parent_invocation_id
      AND plan.status IN ('authorized','running')
      AND child.agent_name=plan.agent_name
      AND child.job_id=plan.job_id
      AND child.stage=plan.stage
      AND child.physical_unit=repair.repair_physical_unit
      AND child.recognition_plan_version='v2'
      AND child.plan_digest=plan.authorized_plan_digest
      AND child.attempt=1
      AND child.status='succeeded'
      AND child.result_digest=NEW.source_physical_result_digest
)
BEGIN
    SELECT RAISE(ABORT, 'repair settlement is detached from succeeded source evidence');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_finalization_insert_guard
BEFORE INSERT ON k12_recognition_layout_finalizations
WHEN NOT EXISTS (
    SELECT 1
    FROM k12_recognition_layout_plans plan
    WHERE plan.plan_id=NEW.plan_id
      AND plan.parent_invocation_id=NEW.parent_invocation_id
      AND plan.status='running'
      AND plan.authorized_plan_digest=NEW.authorized_plan_digest
      AND plan.candidate_exact_set_digest=NEW.candidate_exact_set_digest
      AND NEW.candidate_result_count=(
          SELECT COUNT(*)
          FROM k12_recognition_layout_candidates candidate
          WHERE candidate.plan_id=plan.plan_id
      )
      AND NEW.candidate_result_count=(
          SELECT COUNT(*)
          FROM k12_recognition_layout_candidate_results result
          WHERE result.plan_id=plan.plan_id
      )
      AND NEW.physical_result_count=(
          SELECT COUNT(*)
          FROM k12_model_physical_invocations child
          WHERE child.parent_invocation_id=plan.parent_invocation_id
            AND child.recognition_plan_version='v2'
      )
      AND NEW.physical_result_count=(
          SELECT COUNT(*)
          FROM k12_model_physical_invocations child
          WHERE child.parent_invocation_id=plan.parent_invocation_id
            AND child.recognition_plan_version='v2'
            AND child.status='succeeded'
            AND child.attempt=1
      )
)
BEGIN
    SELECT RAISE(ABORT, 'layout finalization is detached from complete V2 evidence');
END;

CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_candidate_immutable
BEFORE UPDATE ON k12_recognition_layout_candidates
BEGIN
    SELECT RAISE(ABORT, 'recognition layout candidate is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_batch_immutable
BEFORE UPDATE ON k12_recognition_layout_batches
BEGIN
    SELECT RAISE(ABORT, 'recognition layout batch is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_batch_member_immutable
BEFORE UPDATE ON k12_recognition_layout_batch_members
BEGIN
    SELECT RAISE(ABORT, 'recognition layout batch member is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_batch_settlement_immutable
BEFORE UPDATE ON k12_recognition_layout_batch_settlements
BEGIN
    SELECT RAISE(ABORT, 'recognition layout batch settlement is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_candidate_result_immutable
BEFORE UPDATE ON k12_recognition_layout_candidate_results
BEGIN
    SELECT RAISE(ABORT, 'recognition layout candidate result is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_repair_authorization_immutable
BEFORE UPDATE ON k12_recognition_layout_repair_authorizations
BEGIN
    SELECT RAISE(ABORT, 'recognition layout repair authorization is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_repair_settlement_immutable
BEFORE UPDATE ON k12_recognition_layout_repair_settlements
BEGIN
    SELECT RAISE(ABORT, 'recognition layout repair settlement is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_recognition_layout_finalization_immutable
BEFORE UPDATE ON k12_recognition_layout_finalizations
BEGIN
    SELECT RAISE(ABORT, 'recognition layout finalization is immutable');
END;`

func migrateK12RecognitionLayoutPlanV76(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get V76 recognition layout migration connection: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			retErr = errors.Join(
				retErr,
				fmt.Errorf("close V76 recognition layout migration connection: %w", closeErr),
			)
		}
	}()

	var foreignKeys int
	if readErr := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); readErr != nil {
		return fmt.Errorf("read V76 foreign_keys: %w", readErr)
	}
	if _, disableErr := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); disableErr != nil {
		return fmt.Errorf("disable V76 foreign_keys: %w", disableErr)
	}
	defer func() {
		if foreignKeys != 0 {
			if _, restoreErr := conn.ExecContext(
				context.Background(),
				`PRAGMA foreign_keys=ON`,
			); retErr == nil && restoreErr != nil {
				retErr = fmt.Errorf("restore V76 foreign_keys: %w", restoreErr)
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin V76 recognition layout migration: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("roll back V76 recognition layout migration: %w", rollbackErr))
		}
	}()

	physicalExists, err := txTableExists(ctx, tx, "k12_model_physical_invocations")
	if err != nil {
		return fmt.Errorf("check V76 physical invocation table: %w", err)
	}
	if !physicalExists {
		return commitK12RecognitionLayoutPlanV76Version(ctx, tx, recordVersion)
	}
	for _, table := range []string{
		"agents",
		"k12_grading_jobs",
		"k12_model_invocations",
	} {
		exists, checkErr := txTableExists(ctx, tx, table)
		if checkErr != nil {
			return fmt.Errorf("check V76 parent table %s: %w", table, checkErr)
		}
		if !exists {
			return commitK12RecognitionLayoutPlanV76Version(ctx, tx, recordVersion)
		}
	}

	physicalResultExists, err := txTableExists(
		ctx,
		tx,
		"k12_problem_source_recognition_physical_results",
	)
	if err != nil {
		return fmt.Errorf("check V76 source physical result table: %w", err)
	}
	if physicalResultExists {
		sourceResultExists, checkErr := txTableExists(
			ctx,
			tx,
			"k12_problem_source_recognition_results",
		)
		if checkErr != nil {
			return fmt.Errorf("check V76 source result parent: %w", checkErr)
		}
		if !sourceResultExists {
			return commitK12RecognitionLayoutPlanV76Version(ctx, tx, recordVersion)
		}
	}

	physicalHasVersion, err := txColumnExists(
		ctx,
		tx,
		"k12_model_physical_invocations",
		"recognition_plan_version",
	)
	if err != nil {
		return fmt.Errorf("check V76 physical plan version: %w", err)
	}
	physicalHasLayout, err := txTableSQLContains(
		ctx,
		tx,
		"k12_model_physical_invocations",
		"layout_batch_",
	)
	if err != nil {
		return fmt.Errorf("check V76 physical unit constraint: %w", err)
	}

	resultHasVersion := false
	resultHasLayout := false
	if physicalResultExists {
		resultHasVersion, err = txColumnExists(
			ctx,
			tx,
			"k12_problem_source_recognition_physical_results",
			"recognition_plan_version",
		)
		if err != nil {
			return fmt.Errorf("check V76 source result plan version: %w", err)
		}
		resultHasLayout, err = txTableSQLContains(
			ctx,
			tx,
			"k12_problem_source_recognition_physical_results",
			"layout_batch_",
		)
		if err != nil {
			return fmt.Errorf("check V76 source result unit constraint: %w", err)
		}
	}

	needsPhysicalRebuild := !physicalHasVersion || !physicalHasLayout
	needsResultRebuild := physicalResultExists && (!resultHasVersion || !resultHasLayout)
	if needsResultRebuild {
		// 在同一次切换中基于临时父表重建子表，确保子表引用的父表复合键始终不漂移。
		needsPhysicalRebuild = true
	}
	if needsPhysicalRebuild {
		if err := rebuildK12RecognitionPhysicalLedgersV76(
			ctx,
			tx,
			physicalHasVersion,
			resultHasVersion,
			needsResultRebuild,
		); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, k12RecognitionLayoutPlanV76PhysicalPostDDL); err != nil {
		return fmt.Errorf("restore V76 physical invocation indexes and guards: %w", err)
	}
	if physicalResultExists {
		if _, err := tx.ExecContext(ctx, k12RecognitionLayoutPlanV76PhysicalResultPostDDL); err != nil {
			return fmt.Errorf("restore V76 source physical result indexes and guards: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, k12RecognitionLayoutPlanV76AuthorizationDDL); err != nil {
		return fmt.Errorf("create V76 recognition layout authorization ledgers: %w", err)
	}

	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).
		Scan(&violations); err != nil {
		return fmt.Errorf("check V76 recognition layout foreign keys: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("V76 recognition layout found %d foreign-key conflicts", violations)
	}
	return commitK12RecognitionLayoutPlanV76Version(ctx, tx, recordVersion)
}

func rebuildK12RecognitionPhysicalLedgersV76(
	ctx context.Context,
	tx *sql.Tx,
	physicalHasVersion bool,
	resultHasVersion bool,
	rebuildPhysicalResults bool,
) error {
	for _, table := range []string{
		"k12_problem_source_recognition_physical_results_v76",
		"k12_model_physical_invocations_v76",
	} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return fmt.Errorf("drop stale V76 table %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, k12RecognitionLayoutPlanV76PhysicalTableDDL); err != nil {
		return fmt.Errorf("create V76 physical invocation table: %w", err)
	}
	physicalPlanProjection := `'v1','',''`
	if physicalHasVersion {
		physicalPlanProjection = `recognition_plan_version,plan_digest,candidate_exact_set_digest`
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO k12_model_physical_invocations_v76(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			external_request_id,failure_kind,created_at,updated_at,
			recognition_plan_version,plan_digest,candidate_exact_set_digest
		)
		SELECT
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			external_request_id,failure_kind,created_at,updated_at,%s
		FROM k12_model_physical_invocations
	`, physicalPlanProjection)); err != nil {
		return fmt.Errorf("copy V76 physical invocation evidence: %w", err)
	}

	if rebuildPhysicalResults {
		if _, err := tx.ExecContext(ctx, k12RecognitionLayoutPlanV76PhysicalResultTableDDL); err != nil {
			return fmt.Errorf("create V76 source physical result table: %w", err)
		}
		resultPlanProjection := `'v1','',''`
		if resultHasVersion {
			resultPlanProjection = `recognition_plan_version,plan_digest,candidate_exact_set_digest`
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO k12_problem_source_recognition_physical_results_v76(
				work_id,ordinal,parent_invocation_id,physical_invocation_id,
				physical_unit,result_digest,created_at,
				recognition_plan_version,plan_digest,candidate_exact_set_digest
			)
			SELECT
				work_id,ordinal,parent_invocation_id,physical_invocation_id,
				physical_unit,result_digest,created_at,%s
			FROM k12_problem_source_recognition_physical_results
		`, resultPlanProjection)); err != nil {
			return fmt.Errorf("copy V76 source physical result evidence: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`DROP TABLE k12_problem_source_recognition_physical_results`,
		); err != nil {
			return fmt.Errorf("drop legacy V76 source physical result table: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE k12_model_physical_invocations`); err != nil {
		return fmt.Errorf("drop legacy V76 physical invocation table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE k12_model_physical_invocations_v76
		RENAME TO k12_model_physical_invocations
	`); err != nil {
		return fmt.Errorf("activate V76 physical invocation table: %w", err)
	}
	if rebuildPhysicalResults {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE k12_problem_source_recognition_physical_results_v76
			RENAME TO k12_problem_source_recognition_physical_results
		`); err != nil {
			return fmt.Errorf("activate V76 source physical result table: %w", err)
		}
	}
	// 未重建的兼容子表继续通过稳定的规范表名绑定到刚启用的父表。
	return nil
}

func commitK12RecognitionLayoutPlanV76Version(
	ctx context.Context,
	tx *sql.Tx,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit V76 recognition layout migration: %w", err)
	}
	return nil
}

func txTableSQLContains(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	fragment string,
) (bool, error) {
	var ddl sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT sql FROM sqlite_master WHERE type='table' AND name=?
	`, table).Scan(&ddl); err != nil {
		return false, err
	}
	return ddl.Valid && strings.Contains(ddl.String, fragment), nil
}
