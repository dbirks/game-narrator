# Architecture Decision Records

Keep one immutable ADR per architectural decision in `docs/adr/NNNN-brief-name.md`, starting at 0001. Decisions require context, options, consequences, verification, security/compatibility implications, rollback plan and links to tests. Status: `proposed`, `accepted`, `superseded`, `rejected`, `deprecated`. Never delete an accepted ADR: supersede with link to the new one.

## ADR template

```
# ADR-XXXX: Name
Date: YYYY-MM-DD
Status: proposed|accepted|superseded|rejected
Owners: ...
Related requirements/tests: GN-..., GN-T...
Context and constraints:
Decision:
Options considered:
Positive consequences:
Negative consequences / risks:
How to verify (commands and evidence):
Rollback/migration path:
Links/references:
Supersedes / superseded by:
```

Initial ADRs record already-selected user decisions. Unverified device/API details are called out as validation work, not accepted claims. Automation must require an ADR link on PRs involving persistence, provider protocols, deployment, game-specific parser decisions, security boundaries or testing architecture.
