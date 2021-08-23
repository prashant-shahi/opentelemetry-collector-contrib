CREATE MATERIALIZED VIEW signoz_operations
ENGINE SummingMergeTree
PARTITION BY toYYYYMM(date) ORDER BY (date, serviceName, name)
SETTINGS index_granularity=32
POPULATE
AS SELECT
  toDate(timestamp) AS date,
  serviceName,
  name
FROM signoz_index
GROUP BY date, serviceName, name