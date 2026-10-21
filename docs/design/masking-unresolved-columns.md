# Masking refuses what it could not analyze

Status: implemented in this PR · 2026-10-20

To decide whether a result column is sensitive, masking traces it to the base-table columns it
came from. A view is expanded for this: `SELECT phone_prefix5 FROM vw_pg_smtlk_sms` resolves to
`pgsmtlk.pg_smtlk_sms.receiver`, and the policies on that column decide what the user sees. When
the trace fails — the source column is missing from the synced schema — no policy matches it, and
returning the rows anyway would return data that no policy was ever checked against. So the
default is to refuse the query, and `QuerySpan.UnresolvedColumnsError` exists to say that missing
column metadata is not the same as an absence of policies.

The refusal lives in two places, both in `backend/api/v1`: `preMaskingRefusal` before the results
are masked, and `unresolvedMaskingLineage` inside `MaskResults`. Before either runs, the query path
re-syncs the databases the span pointed at and recomputes it (`queryRetry`), so stale metadata
recovers on its own and only a *persistently* unresolvable column reaches the refusal.

Masking being off is not a reason to refuse. The pre-masking refusal used to run unconditionally,
so an instance with masking disabled, unlicensed, or skipped by an unmask grant still lost queries
to a masking-only check. It is now gated on the same `maskingEnabled` as the masking itself.

## The switch, and what it costs

`QueryDataPolicy.allow_unresolved_masking_columns` (workspace-level, off by default, inside
`FEATURE_QUERY_POLICY`) serves the rows instead, logging a warning that names the lineage it could
not resolve. It exists for a data source account scoped so narrowly that the synced schema is
knowingly incomplete: granted on a view, with no privilege on the base table the view reads, so
`information_schema.COLUMNS` hides that column from the sync.

The same switch has a deployment-level form, `ALLOW_UNRESOLVED_MASKING_COLUMNS` in the server's
environment, for an operator who cannot reach the policy — no query-policy license, or no API
access. It applies to every workspace, takes the values `strconv.ParseBool` accepts (`true`, `1`),
and the policy can only add to it, never switch it off. Both are read in one place,
`getEffectiveQueryDataPolicy`, so the rest of the query path sees a single effective answer.

Turning it on is a real weakening, for two reasons worth stating plainly:

- **A statement is masked whole or refused whole.** MySQL's extractor, for one, reports no result
  columns at all for a statement it could not fully resolve, so one unresolved column leaves that
  statement's other columns unmasked too, including columns that do carry a policy.
- **"Invisible to the sync" does not mean "unreachable by users".** A `SQL SECURITY DEFINER` view
  runs with its definer's rights: on MySQL 8.0.33, an account denied `SELECT` on the base table
  (error 1142) still reads the view and receives the column's data. Anything that can create such
  a view — outside Bytebase, on the database itself — can therefore expose a column that masking
  never gets to see.

The alternative that keeps the guarantee is to let the sync see the schema without letting the
account read the data. A column-level `REFERENCES` grant does exactly that:

```sql
GRANT REFERENCES (id, receiver, created_date) ON pgsmtlk.pg_smtlk_sms TO 'ai_query_service'@'%';
```

On MySQL 8.0.33 the table and all three columns then appear in `information_schema.TABLES` and
`.COLUMNS`, so the sync records them and the lineage resolves, while `SELECT receiver FROM
pg_smtlk_sms` stays denied with error 1142. Prefer this over the switch wherever the base table's
privileges are the reason the schema is incomplete.
