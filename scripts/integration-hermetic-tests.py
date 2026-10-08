#!/usr/bin/env python3
"""Run hermetic regressions whose production helpers require integration tags.

Invoke inside the builder image via `just tagged-unit-test`. Exact named PASS
records are required so a changed selector, skip, or missing test fails closed.
No cluster, daemon, release image or credentials are needed by these tests.
"""
import json
import subprocess
import sys

TESTS = {'./test/robustness': ['TestSoakStartIncompleteBodyReconcilesOnlyWithSameKey',
                       'TestNoKeyHealReadErrorPreservesPossibleIdentityAndStatus',
                       'TestHealThroughInterposerIncompleteResponseNeverBlindRetries',
                       'TestSoakQueueDepthRejectsIncompleteEvidence',
                       'TestSoakWaitTerminalRetriesIncompleteEvidence',
                       'TestSoakWaitTerminalUnreadableReasonSurvivesCancellation',
                       'TestDurableSnapshotCannotCertifyUnavailableSQL'],
 './test/lifecycle': ['TestReadEventBacklogCompleteFrameAndIdle',
                      'TestReadEventBacklogRejectsPartialFrameAtIdleAndEOF',
                      'TestReadEventBacklogReturnsParentAndHardLimitErrors',
                      'TestReadEventBacklogPropagatesScannerError',
                      'TestReadEventBacklogAcceptsEOFAfterCompletedFrame',
                      'TestMixedProtocolProbeRejectsValidJSONWithReadErrorAndKeepsRetryReason',
                      'TestMixedProtocolProbeRetainsUnboundedCompleteReader',
                      'TestTaskProofUnavailableMatchesBothCausesWithSameDiagnostic'],
 './test/robustness/cluster': ['TestHTTPDoRejectsOverflowAndRetainsStatusPartialBody',
                               'TestTriggerReadFailureIsUncertainEvenWithValidAcceptedJSON',
                               'TestMutationTransportFailureIsUncertainButPreflightIsDefinitive',
                               'TestInternalPostRequiresCompleteBoundedEvidence',
                               'TestPublicQueryRetainsLegacyCellsAndLimit',
                               'TestQueryLeaseExactGenerationAndMalformedCells',
                               'TestQueryLeaseRejectsWrongIdentityAndShortRows',
                               'TestQueryTaskRecipesRejectsMalformedCounters',
                               'TestQueryPoliciesRetainStatusAndDecodeLabels',
                               'TestQueryUUIDCanonicalizesSQLCellsAndRejectsInvalidIdentity',
                               'TestQueryLeaseAbsentRequiresSuccessfulUnambiguousSQLRead',
                               'TestQueryTaskRecipesKeepsClaimAttemptSeparateFromRetryAttempt',
                               'TestQueryTaskRecipesRequiresCompletePage',
                               'TestResolveUnfannedTaskRecipeRequiresUniqueDurableInstance',
                               'TestQueryCellDigestDistinguishesNullAndOutputMutation'],
 './test/performance': ['TestServerEnvProbeRejectsIncompleteValidEvidence',
                        'TestServerEnvProbeCompleteEvidence',
                        'TestTypedExpectationPresenceKeepsFalseZeroAndEmptyCollections',
                        'TestTypedExpectationsCoverAllTwentyFieldsAndNestedForms',
                        'TestPerformanceCatalogLoadsTypedExpectationsBeforeSelection']}


def main():
    for package, expected in TESTS.items():
        selector = "^(" + "|".join(expected) + ")$"
        result = subprocess.run(
            ["go", "test", "-json", "-race", "-count=1", "-tags=integration",
             "-timeout=3m", "-run", selector, package],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        passed = set()
        for line in result.stdout.splitlines():
            event = json.loads(line)
            if event.get("Action") == "pass" and event.get("Test") in expected:
                passed.add(event["Test"])
        missing = set(expected) - passed
        if result.returncode or missing:
            print(f"{package}: required named passes missing: {sorted(missing)}", file=sys.stderr)
            return 1
        print(f"{package}: {len(expected)} required hermetic regressions passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
