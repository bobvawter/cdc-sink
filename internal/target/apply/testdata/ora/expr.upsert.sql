MERGE INTO "schema"."table" USING (
WITH data ("pk0","pk1","val0","val1","has_default") AS (
SELECT CAST(:1 AS VARCHAR(256)), CAST(:ref2+:ref2 AS INT), CAST('fixed' AS VARCHAR(256)), CAST(:ref3||'foobar' AS VARCHAR(256)), CASE WHEN :4 = 1 THEN CAST(:5 AS INT8) ELSE expr() END FROM DUAL
)
SELECT * FROM data) x ON ("schema"."table"."pk0" = x."pk0" AND "schema"."table"."pk1" = x."pk1")
WHEN NOT MATCHED THEN INSERT ("pk0","pk1","val0","val1","has_default") VALUES (x."pk0", x."pk1", x."val0", x."val1", x."has_default")
WHEN MATCHED THEN UPDATE SET "val0" = x."val0", "val1" = x."val1", "has_default" = x."has_default"