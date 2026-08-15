BEGIN;

CREATE TEMP TABLE duplicate_file_entries ON COMMIT DROP AS
WITH ranked AS (
    SELECT id, owner_id, object_id,
           first_value(id) OVER (
               PARTITION BY owner_id, object_id
               ORDER BY created_at ASC, id ASC
           ) AS canonical_id,
           row_number() OVER (
               PARTITION BY owner_id, object_id
               ORDER BY created_at ASC, id ASC
           ) AS position
    FROM file_entries
    WHERE deleted_at IS NULL
)
SELECT id AS duplicate_id, canonical_id, owner_id, object_id
FROM ranked
WHERE position > 1;

UPDATE shares AS s
SET file_entry_id = d.canonical_id,
    updated_at = now()
FROM duplicate_file_entries AS d
WHERE s.file_entry_id = d.duplicate_id;

UPDATE share_imports AS i
SET imported_entry_id = d.canonical_id
FROM duplicate_file_entries AS d
WHERE i.imported_entry_id = d.duplicate_id;

UPDATE upload_sessions AS u
SET completed_entry_id = d.canonical_id,
    updated_at = now()
FROM duplicate_file_entries AS d
WHERE u.completed_entry_id = d.duplicate_id;

UPDATE users AS u
SET used_logical_bytes = GREATEST(u.used_logical_bytes - removed.bytes, 0),
    updated_at = now()
FROM (
    SELECT d.owner_id, sum(o.size_bytes) AS bytes
    FROM duplicate_file_entries AS d
    JOIN file_objects AS o ON o.id = d.object_id
    GROUP BY d.owner_id
) AS removed
WHERE u.id = removed.owner_id;

UPDATE file_entries AS e
SET deleted_at = now(),
    version = e.version + 1,
    updated_at = now()
FROM duplicate_file_entries AS d
WHERE e.id = d.duplicate_id;

UPDATE file_objects AS o
SET reference_count = (
        SELECT count(*)
        FROM file_entries AS e
        WHERE e.object_id = o.id AND e.deleted_at IS NULL
    ),
    updated_at = now()
WHERE o.id IN (SELECT DISTINCT object_id FROM duplicate_file_entries);

CREATE UNIQUE INDEX uq_file_entries_owner_object_active
    ON file_entries(owner_id, object_id)
    WHERE deleted_at IS NULL;

COMMIT;
