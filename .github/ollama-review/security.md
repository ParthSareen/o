# Selected profile: focused security review

For this run, report only bugs with a concrete security consequence. Apply the
same evidence, change-attribution and output rules above. General correctness,
performance and UI issues belong in the combined general profile unless they
also create a demonstrated security impact.

Prioritize attacker-controlled inputs, authorization and tenant boundaries,
sensitive data exposure, injection, unsafe file/network access and privileged
CI or agent execution. Trace the entry point, relevant existing defenses and
security-sensitive operation with the repository tools before making a claim.
Missing context is not evidence of a vulnerability. A secure or unrelated change
should submit an empty findings array. Do not assert that a clean result is a
security audit, penetration test or assurance of absence of vulnerabilities.
