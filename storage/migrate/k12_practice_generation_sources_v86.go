package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12PracticeGenerationSourcesV86 为共享逐题任务增加稳定来源身份，
// 并把最新旧积累默写记录导入只读审计历史。
var K12PracticeGenerationSourcesV86 = Migration{
	Version:     86,
	Description: "K12 shared source practice generation jobs",
	AtomicFunc:  migrateK12PracticeGenerationSourcesV86,
}

func migrateK12PracticeGenerationSourcesV86(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 practice generation sources V86 migration: %w", err)
	}
	defer tx.Rollback()

	for _, column := range []struct {
		name string
		def  string
	}{
		{name: "source_kind", def: "TEXT NOT NULL DEFAULT ''"},
		{name: "source_id", def: "TEXT NOT NULL DEFAULT ''"},
		{name: "source_version", def: "INTEGER NOT NULL DEFAULT 0"},
	} {
		has, checkErr := txColumnExists(
			ctx, tx, "k12_practice_generation_jobs", column.name,
		)
		if checkErr != nil {
			return fmt.Errorf(
				"inspect k12_practice_generation_jobs.%s: %w", column.name, checkErr,
			)
		}
		if has {
			continue
		}
		if _, alterErr := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE k12_practice_generation_jobs ADD COLUMN %s %s`,
			column.name, column.def,
		)); alterErr != nil {
			return fmt.Errorf(
				"add k12_practice_generation_jobs.%s: %w", column.name, alterErr,
			)
		}
	}

	// 旧任务没有冻结来源版本，只有最新一条可成为当前来源身份；更早记录继续按
	// 旧字段保留查询能力，不为它们虚构来源版本。
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_generation_jobs AS job
		SET source_kind='mistake',
			source_id=job.source_mistake_id,
			source_version=COALESCE((
				SELECT mistake.version FROM k12_mistakes AS mistake
				WHERE mistake.record_id=job.source_mistake_id
				  AND mistake.agent_name=job.agent_name
			),0)
		WHERE job.scope='single' AND trim(job.source_mistake_id)!=''
		  AND trim(job.source_kind)=''
		  AND NOT EXISTS (
			SELECT 1 FROM k12_practice_generation_jobs AS newer
			WHERE newer.agent_name=job.agent_name
			  AND newer.scope='single'
			  AND newer.source_mistake_id=job.source_mistake_id
			  AND (newer.created_at>job.created_at OR
			      (newer.created_at=job.created_at AND
			       newer.generation_job_id>job.generation_job_id))
		  )`); err != nil {
		return fmt.Errorf("backfill canonical mistake source identity: %w", err)
	}

	legacyExists, err := txTableExists(
		ctx, tx, "k12_accumulation_dictation_generations",
	)
	if err != nil {
		return fmt.Errorf("inspect legacy accumulation generation table: %w", err)
	}
	if legacyExists {
		if _, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_generation_jobs(
			generation_job_id,agent_name,idempotency_key,request_digest,scope,
			variants_per_source,difficulty,total,textbook,status,result_set_id,
			result_item_ids_json,deduplicated_count,failure_reason,source_mistake_id,
			source_mistake_summary,request_snapshot_json,route_snapshot_json,attempt,
			generation_output_json,generation_output_attempt,validation_output_json,
			validation_output_attempt,retired_at,retired_reason,created_at,updated_at,
			source_kind,source_id,source_version
		)
		SELECT legacy.generation_id,legacy.agent_name,legacy.command_key,
			legacy.request_digest,'single',1,'same','1','',
			CASE WHEN legacy.status='re_add' THEN 'committed' ELSE legacy.status END,
			CASE WHEN trim(legacy.practice_item_id)='' THEN '' ELSE COALESCE((
				SELECT item.set_record_id FROM k12_practice_set_items AS item
				WHERE item.item_id=legacy.practice_item_id
				ORDER BY item.set_record_id LIMIT 1
			),'') END,
			json_array(CASE WHEN trim(legacy.practice_item_id)!=''
				THEN legacy.practice_item_id ELSE 'dictation-' || legacy.generation_id END),
			0,legacy.failure_reason,'',
			CASE WHEN json_valid(legacy.source_snapshot_json)
				THEN COALESCE(json_extract(legacy.source_snapshot_json,'$.content'),'')
				ELSE '' END,
			legacy.source_snapshot_json,legacy.route_snapshot_json,
			CASE WHEN legacy.attempt<1 THEN 1 ELSE legacy.attempt END,
			'',0,'',0,
			CASE WHEN legacy.status='re_add'
				THEN CASE WHEN legacy.updated_at>0 THEN legacy.updated_at ELSE legacy.created_at END
				ELSE 0 END,
			CASE WHEN legacy.status='re_add' THEN 'removed' ELSE '' END,
			legacy.created_at,legacy.updated_at,'accumulation',legacy.accumulation_id,
			accumulation.row_version
		FROM k12_accumulation_dictation_generations AS legacy
		JOIN k12_accumulations AS accumulation
		  ON accumulation.record_id=legacy.accumulation_id
		 AND accumulation.agent_name=legacy.agent_name
		WHERE NOT EXISTS (
			SELECT 1 FROM k12_accumulation_dictation_generations AS newer
			WHERE newer.agent_name=legacy.agent_name
			  AND newer.accumulation_id=legacy.accumulation_id
			  AND (newer.created_at>legacy.created_at OR
			      (newer.created_at=legacy.created_at AND
			       newer.generation_id>legacy.generation_id))
		)`); err != nil {
			return fmt.Errorf("import legacy accumulation generation audit: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_set_items AS item
			SET generation_job_id=(
				SELECT legacy.generation_id
				FROM k12_accumulation_dictation_generations AS legacy
				JOIN k12_practice_generation_jobs AS job
				  ON job.generation_job_id=legacy.generation_id
				WHERE legacy.agent_name=job.agent_name
				  AND legacy.practice_item_id=item.item_id
				  AND legacy.status='committed'
				ORDER BY legacy.updated_at DESC,legacy.generation_id DESC LIMIT 1
			),
			generation_status='ready'
			WHERE trim(item.generation_job_id)=''
			  AND EXISTS (
				SELECT 1 FROM k12_accumulation_dictation_generations AS legacy
				JOIN k12_practice_generation_jobs AS job
				  ON job.generation_job_id=legacy.generation_id
				WHERE legacy.agent_name=job.agent_name
				  AND legacy.practice_item_id=item.item_id
				  AND legacy.status='committed'
			  )`); err != nil {
			return fmt.Errorf("attach migrated accumulation item to shared job: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DROP INDEX IF EXISTS idx_k12_single_generation_active_source;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_legacy_single_generation_active_source
			ON k12_practice_generation_jobs(agent_name,source_mistake_id)
			WHERE scope='single' AND trim(source_kind)=''
			  AND trim(source_mistake_id)!='' AND retired_at=0
			  AND status IN ('queued','generating','validating');
		CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_practice_generation_source_identity
			ON k12_practice_generation_jobs(
				agent_name,source_kind,source_id,source_version
			)
			WHERE scope='single' AND trim(source_kind)!='' AND trim(source_id)!='';
		CREATE INDEX IF NOT EXISTS idx_k12_practice_generation_source_history
			ON k12_practice_generation_jobs(
				agent_name,source_kind,source_id,source_version,created_at DESC
			)
			WHERE scope='single' AND trim(source_kind)!='' AND trim(source_id)!='';
		DROP INDEX IF EXISTS idx_k12_practice_generation_item_job;
		CREATE INDEX IF NOT EXISTS idx_k12_practice_generation_item_job_lookup
			ON k12_practice_set_items(generation_job_id)
			WHERE trim(generation_job_id)!='';
		CREATE TRIGGER IF NOT EXISTS k12_practice_generation_source_immutable
		BEFORE UPDATE OF source_kind,source_id,source_version
		ON k12_practice_generation_jobs
		BEGIN
			SELECT RAISE(ABORT, 'practice generation source identity is immutable');
		END;
		CREATE TRIGGER IF NOT EXISTS k12_single_practice_generation_item_unique_insert
		BEFORE INSERT ON k12_practice_set_items
		WHEN trim(NEW.generation_job_id)!=''
		 AND (SELECT scope FROM k12_practice_generation_jobs
		      WHERE generation_job_id=NEW.generation_job_id)='single'
		 AND EXISTS (SELECT 1 FROM k12_practice_set_items AS existing
		             WHERE existing.generation_job_id=NEW.generation_job_id)
		BEGIN
			SELECT RAISE(ABORT, 'single practice generation job already has an item');
		END;
		CREATE TRIGGER IF NOT EXISTS k12_single_practice_generation_item_unique_update
		BEFORE UPDATE OF generation_job_id ON k12_practice_set_items
		WHEN trim(NEW.generation_job_id)!=''
		 AND (SELECT scope FROM k12_practice_generation_jobs
		      WHERE generation_job_id=NEW.generation_job_id)='single'
		 AND EXISTS (SELECT 1 FROM k12_practice_set_items AS existing
		             WHERE existing.generation_job_id=NEW.generation_job_id
		               AND existing.rowid!=OLD.rowid)
		BEGIN
			SELECT RAISE(ABORT, 'single practice generation job already has an item');
		END;`); err != nil {
		return fmt.Errorf("create shared practice generation source guards: %w", err)
	}
	if legacyExists {
		if _, err := tx.ExecContext(ctx, `
			CREATE TRIGGER IF NOT EXISTS k12_accum_dictation_audit_no_insert
			BEFORE INSERT ON k12_accumulation_dictation_generations
			BEGIN
				SELECT RAISE(ABORT, 'legacy accumulation generation is audit only');
			END;
			CREATE TRIGGER IF NOT EXISTS k12_accum_dictation_audit_no_update
			BEFORE UPDATE ON k12_accumulation_dictation_generations
			BEGIN
				SELECT RAISE(ABORT, 'legacy accumulation generation is audit only');
			END;
			CREATE TRIGGER IF NOT EXISTS k12_accum_dictation_audit_no_delete
			BEFORE DELETE ON k12_accumulation_dictation_generations
			BEGIN
				SELECT RAISE(ABORT, 'legacy accumulation generation is audit only');
			END;`); err != nil {
			return fmt.Errorf("freeze legacy accumulation generation audit: %w", err)
		}
	}

	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 practice generation sources V86 migration: %w", err)
	}
	return nil
}
