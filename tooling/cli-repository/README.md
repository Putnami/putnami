# @putnami/cli-repository

The CLI's tests that read this repository's own files: the manifests of every
project and extension, the decisions, the templates, the agent content, the
first-party extension sources and the API contracts. They load those files
with the CLI's code, so they cannot join the dependency-free text checks of
[`@putnami/repository-checks`](../repository-checks/README.md).

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task
(`options.test.filePatterns` folds into the key of every `test~*` task). While
these tests ran in `@putnami/cli`, its test inputs listed about 120 patterns
over the whole repository, so a one-line change to any project's
`putnami.json` re-ran the CLI's whole suite of about 5,000 tests. Here they
key on the files they read, and the CLI's suite keys on its own sources and
fixtures.

A change to the CLI's sources still runs both suites: this project depends on
the CLI, and its inputs hold the whole `tooling/cli` tree.

## How it runs the tests

The tests stay in the CLI's packages, beside the unexported code they check.
Each one calls `repotest.Require(t)` as its first statement, which skips it
unless `PUTNAMI_CLI_REPOSITORY_TESTS=1`. `TestTheCLITestsThatReadTheRepository`
finds the marked tests by parsing the CLI's `_test.go` files, runs them with
`go test` over their packages with that variable set, and reports each one as
a subtest. `TestTheMarkerIsTheFirstStatementOfATestFunction` refuses a marker
anywhere else. The helper functions of the CLI's tests that find the
repository root call `repotest.Reads(t)`, which fails a test that reaches one
unmarked. The `repoRoot` globals a `TestMain` sets in the end-to-end packages
and relative paths to sibling projects are not guarded: review keeps the
tests that use them marked.

A spec proof is reported by the project whose tree declares it. The marked
tests write their `spectest.Proves` fragments to a directory of their own;
each subtest declares the checks its marked test proved, and passes, fails or
skips with it, so the CLI's features keep their evidence from a direct
dependent.

The module path is `go.putnami.dev/tooling/cli/repository`, a **child** of the
CLI's module path, so this module may import
`go.putnami.dev/tooling/cli/internal/...`. Nothing publishes this module.

When this project's tests run under the race detector, the run passes
`-race` to the marked tests: in its own race step, when its own Go files
change, or in a run with `--race`. A CLI change alone does not race them, and
the CLI's `race-changed` step no longer does either (ADR 0001).

## The tests it runs

`TestTheInventoryListsEveryMarkedTest` keeps this list equal to the marked
tests.

<!-- marked-tests:start -->
- `internal/bootstrap.TestPutnamiwBunCache_FollowsTheBunOfTheMachine`
- `internal/bootstrap.TestPutnamiwDownload_CleansStagingOnFailure`
- `internal/bootstrap.TestPutnamiwDownload_HonorsTheLockedCLIDigest`
- `internal/bootstrap.TestPutnamiwDownload_PublishesSharedBlobAndStampsRecency`
- `internal/bootstrap.TestPutnamiwDownload_RefusesAPlaintextRegistry`
- `internal/bootstrap.TestPutnamiwDownload_ReusesExistingBlob`
- `internal/bootstrap.TestPutnamiwDownload_RunsOnlyTheArchiveTheRegistryAdvertised`
- `internal/bootstrap.TestPutnamiwDownloads_BoundTheirConnections`
- `internal/bootstrap.TestPutnamiwPrintEngine_FailsWithNothingOnStdout`
- `internal/bootstrap.TestPutnamiwPrintEngine_InstallsTheLockedGoBeforeExtensionsPrepare`
- `internal/bootstrap.TestPutnamiwPrintEngine_LeavesTheWorkspaceLinkAlone`
- `internal/bootstrap.TestPutnamiwPrintEngine_PrintsTheBlobAndRunsOnlyExtensionsPrepare`
- `internal/bootstrap.TestPutnamiwSHA256OfFallsBackToOpenSSL`
- `internal/bootstrap.TestPutnamiwSourceKey_BootstrapForcesRebuild`
- `internal/bootstrap.TestPutnamiwSourceKey_BuildEnvironmentStaysOutOfTheCLI`
- `internal/bootstrap.TestPutnamiwSourceKey_BuildsFromTheWorkspaceWhateverTheCallerDirectory`
- `internal/bootstrap.TestPutnamiwSourceKey_ConcurrentRelinkLeavesAnEarlierPinIntact`
- `internal/bootstrap.TestPutnamiwSourceKey_DownloadedGoMustMatchItsSHA256`
- `internal/bootstrap.TestPutnamiwSourceKey_EditOutsideToolingCliForcesRebuild`
- `internal/bootstrap.TestPutnamiwSourceKey_ExecsTheBlobAndExportsTheSessionPin`
- `internal/bootstrap.TestPutnamiwSourceKey_FailedBuildNeverRunsACachedBlob`
- `internal/bootstrap.TestPutnamiwSourceKey_KeylessRunUnsetsAnInheritedPin`
- `internal/bootstrap.TestPutnamiwSourceKey_NonGitWorkspaceUsesTheMtimeGate`
- `internal/bootstrap.TestPutnamiwSourceKey_OlderSystemGoBuildsWithTheManagedInstall`
- `internal/bootstrap.TestPutnamiwSourceKey_RepointedLinkIsNotTrusted`
- `internal/bootstrap.TestPutnamiwSourceKey_ScratchFilesStayInAPrivateDirectory`
- `internal/bootstrap.TestPutnamiwSourceKey_SecondRunReusesTheBlob`
- `internal/bootstrap.TestPutnamiwSourceKey_SiblingWorktreesShareOneBuild`
- `internal/bootstrap.TestPutnamiwSourceKey_UntrackedFileForcesRebuild`
- `internal/bootstrap.TestPutnamiwSourceKey_VCSStampFollowsGit`
- `internal/bootstrap.TestPutnamiwStaleness_ConsumerWorkspaceRunsPin`
- `internal/bootstrap.TestPutnamiwStaleness_RebuildsOverStalePin`
- `internal/bootstrap.TestPutnamiwStaleness_RunsFreshPinAsIs`
- `internal/bootstrap.TestSeedSourceTreeIsTheSameOnAForeignMachine`
- `internal/cli.TestCommandTraits_EveryFieldHasANonTestReadSite`
- `internal/cli.TestDevVersionCommandsMatchTheRecordedCore`
- `internal/cli.TestSDDExtensionSpeaksOnlyTheWire`
- `internal/cli.TestSDDHumanOutputMatchesTheRecordedBuiltIn`
- `internal/cli.TestSDDSubcommandsMatchTheRecordedBuiltIn`
- `internal/cli.TestTheLocalProviderAnswersBothEntryPaths`
- `internal/cli.TestTheLocalProviderReadsASessionWithMissionsStatus`
- `internal/cli/e2e/bazeldelegation.TestAChangeToOneBazelPackageRunsItAndItsDependentsThroughBazel`
- `internal/cli/e2e/bazeldelegation.TestTheWorkspaceAdoptsOnlyThePackagesBazelLoads`
- `internal/cli/e2e/clientgencache.TestACachedTypeScriptClientGenerationRestoresTheTreeItsExecutionWrote`
- `internal/cli/e2e/credentialcustody.TestReleaseSetE2EPublicationV1UploadsThroughTheProviderAndReleasesForwardOnly`
- `internal/cli/e2e/devversion.TestTheContributorPrepareScriptIsTheDevOneUnderItsOwnNames`
- `internal/cli/e2e/devversion.TestVersionGetRunsThroughTheDevExtensionBuiltFromSource`
- `internal/cli/e2e/devworkload.TestComposeRestartsAWatchedTargetWithItsServeEnvironment`
- `internal/cli/e2e/devworkload.TestComposeTearsDownOnceTheCLIIsKilled`
- `internal/cli/e2e/devworkload.TestDevWorkloadCommandsMatchTheRecordedCore`
- `internal/cli/e2e/moonadoption.TestAChangeToOneMoonProjectRunsItAndItsDependentsThroughMoon`
- `internal/cli/e2e/moonadoption.TestAnEdgeTheMoonSyncWritesIsNoPhantom`
- `internal/cli/e2e/moonadoption.TestAnImportWhoseInstallFailsFailsInit`
- `internal/cli/e2e/moonadoption.TestInitFromMoonInImportModeMapsTheProjectsAndPassesLintTestBuild`
- `internal/cli/e2e/moonadoption.TestTheWorkspaceAdoptsOnlyTheProjectsMoonLists`
- `internal/cli/e2e/nxdelegation.TestAChangeToOneNxProjectRunsItAndItsDependentsThroughNx`
- `internal/cli/e2e/nxdelegation.TestADelegatedNxWorkspaceImportsPricingAndPassesLintTestBuild`
- `internal/cli/e2e/nxdelegation.TestAnImportedNxProjectTakesItsPackageName`
- `internal/cli/e2e/nxdelegation.TestAnImportedNxTestWaitsForTheNxBuildAndFoldsItsKey`
- `internal/cli/e2e/nxdelegation.TestAnNxPnpmWorkspaceImportsAndRunsItsOwnNodeTools`
- `internal/cli/e2e/nxdelegation.TestTheWorkspaceAdoptsOnlyTheProjectsTheNxGraphLists`
- `internal/cli/e2e/orchestration.TestATscOutputGitDoesNotIgnoreIsRefusedOnceItKeysTheBuild`
- `internal/cli/e2e/orchestration.TestEveryExtensionBuiltHereIsCounted`
- `internal/cli/e2e/orchestration.TestTheExtensionCounts`
- `internal/cli/e2e/pantsdelegation.TestAChangeToOnePantsPackageRunsItAndItsDependentsThroughPants`
- `internal/cli/e2e/pantsdelegation.TestANestedPantsPackageRunsWithItsEdgesAcrossScopes`
- `internal/cli/e2e/pantsdelegation.TestARootRequirementOrAStrayLockfileRunsThePackagesThatUseIt`
- `internal/cli/e2e/pantsdelegation.TestASymlinkedPantsDirectoryPassesValidateRightAfterTheSync`
- `internal/cli/e2e/portable.TestPortableExecutingEngineRefusesTheShippedDevWorkloads`
- `internal/cli/e2e/rushdelegation.TestAChangeToOneRushProjectRunsRushBuildOnItAndItsDependents`
- `internal/cli/e2e/rushdelegation.TestTheWorkspaceAdoptsOnlyTheProjectsRushLists`
- `internal/cli/e2e/rushimport.TestAnEdgeTheRushSyncWritesIsNoPhantom`
- `internal/cli/e2e/rushimport.TestAnImportedRushRepositoryPassesLintTestBuildWithTheGraphObserveReads`
- `internal/cli/e2e/rushimport.TestValidateAgainstABaselineFailsOnANewEdgeInAnImportedRushRepository`
- `internal/cli/e2e/tscbuild.TestATscLibraryBuildsBeforeItsDependentAndItsOutDirRestoresFromTheCache`
- `internal/cli/e2e/turbodelegation.TestAChangeToOneTurboPackageRunsItAndItsDependentsThroughTurbo`
- `internal/cli/e2e/turbodelegation.TestADelegatedTurborepoImportsPricingAndPassesLintTestBuild`
- `internal/cli/e2e/turbodelegation.TestATurborepoPnpmWorkspaceImportsAndRunsItsOwnNodeTools`
- `internal/cli/e2e/turbodelegation.TestAnImportThatMovesNoTaskLeavesTheLockfileAsItWas`
- `internal/cli/e2e/turbodelegation.TestAnImportedTurborepoMovesOnlyTheTasksTypeScriptRunsAndListsTheOthers`
- `internal/cli/e2e/turbodelegation.TestTheWorkspaceAdoptsOnlyThePackagesThePnpmWorkspaceLists`
- `internal/cli/e2e/workspacewarnings.TestADevCommandEnvelopeCarriesTheWarningsOfItsOwnLoad`
- `internal/commands/agentctx.TestChangeFinalizeMatchesProposalsFinalize`
- `internal/commands/agentctx.TestChangeVerifyThroughPutnamiRunsTheContributorExecutable`
- `internal/commands/agentctx.TestCommittedRecipeReferenceMatchesTheSampleIndexes`
- `internal/commands/agentctx.TestConstraintsNameQualifyAsLocalProof`
- `internal/commands/agentctx.TestContributorBuildCheckAndDesignStopWithoutAWorkspace`
- `internal/commands/agentctx.TestContributorBuildIsAShortCoreAndOneReferencePerStage`
- `internal/commands/agentctx.TestContributorBuildLoopChecksDeclarationsBeforeThePush`
- `internal/commands/agentctx.TestContributorBuildLoopPushesDeclarationsBeforeCode`
- `internal/commands/agentctx.TestContributorContentIsProviderNeutral`
- `internal/commands/agentctx.TestContributorContentPolicyRefusesProviderSpecificContent`
- `internal/commands/agentctx.TestContributorContentRendersEveryEntryPointOnBothHosts`
- `internal/commands/agentctx.TestContributorContextSkillReadsObserveWithoutAWorkspace`
- `internal/commands/agentctx.TestContributorContextSkillReadsTheCIOfAChange`
- `internal/commands/agentctx.TestContributorEpicLaunchesChildrenAndNeverCodes`
- `internal/commands/agentctx.TestContributorExplicitOnlySkillsDeclareItOnBothHosts`
- `internal/commands/agentctx.TestContributorInstructionsNameProfilesNotModels`
- `internal/commands/agentctx.TestContributorNamesTheWorkspaceFreeSkills`
- `internal/commands/agentctx.TestContributorPackagedAndSourceConsumerMigration`
- `internal/commands/agentctx.TestContributorResourceLinks`
- `internal/commands/agentctx.TestContributorReviewCycleBlocksOnP1AndP2`
- `internal/commands/agentctx.TestContributorReviewMetadataKeepsTheReviewQualityFloor`
- `internal/commands/agentctx.TestContributorShipsOnlyCanonicalEntriesAndActiveUtilities`
- `internal/commands/agentctx.TestContributorSpecSkillEditsTheExternalSpec`
- `internal/commands/agentctx.TestContributorSpecSkillWritesTheRepositoryFormatWithoutAWorkspace`
- `internal/commands/agentctx.TestContributorUserScopeSkillsDeferToTheWorkspaceCopy`
- `internal/commands/agentctx.TestContributorWorkflowProfilesReplaceTiersAndWorkers`
- `internal/commands/agentctx.TestEveryLinkedRecipeIsAPublishedPage`
- `internal/commands/agentctx.TestFinalizeTwinIsTheSameOnAForeignMachine`
- `internal/commands/agentctx.TestFirstPartySuccessorsMatchTheContributorManifest`
- `internal/commands/agentctx.TestGeneratedGuidanceMatchesRepositoryCIDocument`
- `internal/commands/agentctx.TestLocalAgentContentMatchesCommittedRoot`
- `internal/commands/agentctx.TestManagedAuditProposalScanUsesNativeLanguage`
- `internal/commands/lifecycle.TestInitLanguagesFollowTheExtensionManifests`
- `internal/commands/lifecycle.TestInitStartersNameTheirExtension`
- `internal/engine.TestNarrowToTaskScopes_KeepsWhatEveryFirstPartyCommandKept`
- `internal/engine.TestUnpublishedProjects_CloudArchivePublishIsTheProjectsOwnJob`
- `internal/extension.TestFirstPartyDeclaredTasksConform`
- `internal/extension.TestFirstPartyJobsKeepTheirTraitsUnderADevelopmentVocabulary`
- `internal/extension.TestFirstPartyLongLivedServeTasksAreUncacheableProcesses`
- `internal/extension.TestFirstPartyManifestsLoadUnderTheCurrentContract`
- `internal/extension.TestFirstPartyManifestsPassThePublishGate`
- `internal/extension.TestFirstPartyShellJobsStampRuntimeEventsV2`
- `internal/extension.TestTheDevVocabularyDeclaresEveryFirstPartyKind`
- `internal/extension.TestTheFirstPartyBatchableTasksKeepTheirTallyAndPhase`
- `internal/extension.TestTheShippedDevExtensionSatisfiesALanguageDependencyByPath`
- `internal/jobs/cachekey.TestFirstPartyCaptureReachability`
- `internal/jobs/cachekey.TestGoBuildKeyIgnoresADependencyForeignOptions`
- `internal/jobs/cachekey.TestGoBuildKeyIgnoresAnotherExtensionsOptions`
- `internal/jobs/cachekey.TestGoCompileKeyIgnoresTestFiles`
- `internal/jobs/cachekey.TestGoDescribeKeyIgnoresADependencyTestFile`
- `internal/jobs/cachekey.TestGoSourceKeyIgnoresGoFilesUnderTheGeneratedTree`
- `internal/jobs/cachekey.TestGoTestKeyReadsTestFiles`
- `internal/jobs/cachekey.TestGoWorkspaceModuleFilesMoveRealTaskCacheKey`
- `internal/jobs/cachekey.TestTypeScriptLintCheckOnlyCacheKeyIgnoresGeneratedFiles`
- `internal/jobs/planner.TestAConsumerOnlySelectionPlansTheGenerationOfItsClosureClients`
- `internal/jobs/planner.TestAContractEditMovesTheKeysOfTheConsumerJobs`
- `internal/jobs/planner.TestAGoSiteProjectPlansTheGoBuildBesideTheSiteContentGenerateStep`
- `internal/jobs/planner.TestAPlainSiteContentProjectPlansValidateAndPackage`
- `internal/jobs/planner.TestBuildPlansNoCloudJobForAProjectThatUsesNoCloudFeature`
- `internal/jobs/planner.TestCLIValidationLintStaysOffTheTestCriticalChain`
- `internal/jobs/planner.TestClientgenKeyIgnoresTheDescribeOwnedTrees`
- `internal/jobs/planner.TestClientgenStartKeyEqualsThePrecomputedKeyWhenDescribeWrites`
- `internal/jobs/planner.TestGoManifestPlansTheRaceStepOnTheTouchedProject`
- `internal/jobs/planner.TestInactiveChannels_AJobWithNoChannelAlwaysPackages`
- `internal/jobs/planner.TestInactiveChannels_IgnoresOtherCommandsAndUnmatchedProjects`
- `internal/jobs/planner.TestInactiveChannels_NamesAProjectWithEveryChannelOff`
- `internal/jobs/planner.TestNativeValidateGuardIsDisableable`
- `internal/jobs/planner.TestNativeValidateGuardIsNotPlannedForAnotherCommand`
- `internal/jobs/planner.TestNativeValidatePlansNoGeneration`
- `internal/jobs/planner.TestNativeValidatePlansOneClientgenGuardForAnySelection`
- `internal/jobs/planner.TestPackageChannels_AreTheFlagsAStepIsPlannedOn`
- `internal/jobs/planner.TestPlan_APublishJobExposesItsTasksPublishChannels`
- `internal/jobs/planner.TestPlan_DependentsWaitForTheGeneratedClientWriter`
- `internal/jobs/planner.TestPlan_GoInfraOnlyOrdersAfterCompile`
- `internal/jobs/planner.TestPlan_GoLibraryCompileUsesInvocationAndProjectFacts`
- `internal/jobs/planner.TestPlan_GoPackageWithoutARoutedImageIsUnchanged`
- `internal/jobs/planner.TestPlan_GoPackageYieldsTheImageToTheRoutedPackager`
- `internal/jobs/planner.TestPlan_PlannerConditionCommandSetIsOrderIndependent`
- `internal/jobs/planner.TestProviderGenerationNeverOverlapsATestOfItsClient`
- `internal/jobs/planner.TestRealTestPipelinesLeaveTheCommandLayerOutOfTheirBuildSteps`
- `internal/jobs/planner.TestSharedExecutions_AnOrderingStepPassesItsProducersThrough`
- `internal/jobs/planner.TestSharedExecutions_DependentsWaitForAnotherCommandsGenerator`
- `internal/jobs/planner.TestSharedExecutions_RealGoManifest`
- `internal/jobs/planner.TestSharedExecutions_RealGoManifestLeavesNoConflictUnordered`
- `internal/jobs/planner.TestSharedExecutions_RealGoManifestWithPackage`
- `internal/jobs/planner.TestTheCLIProjectTunesItsRaceStepDeadline`
- `internal/jobs/planner.TestTheGeneratedSiteContentSelectionFingerprintFollowsTheGeneratorInputs`
- `internal/jobs/planner.TestTheGeneratedSiteContentValidationKeyFoldsTheGenerateKey`
- `internal/jobs/planner.TestTheStepsThatReadAGeneratedSiteContentSectionWaitForItsGenerateStep`
- `internal/jobs/publication.TestARoutedImageKeepsTheDeploymentMemberKeyable`
- `internal/jobs/publication.TestReleaseSetScopePlanningResolvesTheShippedGoOwner`
- `internal/jobs/publication.TestReleaseSetScopePlanningResolvesTheShippedNpmOwner`
- `internal/jobs/publication.TestTheDeploymentSelectionFingerprintFollowsTheDeclarationInputs`
- `internal/jobs/scheduler.TestEveryUncachedTaskHasAnEffectACacheEntryCannotReplay`
- `internal/jobs/scheduler.TestNoCacheKeyReadsAnotherTasksLiteralOutput`
- `internal/jobs/scheduler.TestSourceBoundExecutionAfterAnInRunRewriteReadsTheCurrentBinding`
- `internal/jobs/scheduler.TestSourceBoundExecutionSettlesAtMostOnce`
- `internal/jobs/scheduler.TestSourceBoundExecutionThatMovesItsBindingSettlesAtTheBindingItLeaves`
- `internal/jobs/scheduler.TestSourceBoundExecutionWithoutTheCacheSettlesToo`
- `internal/jobs/scheduler.TestSourceBoundTasksOfOneProjectTakeTurns`
- `internal/jobs/scheduler.TestSourceBoundTestOnlyEditStoresBesideTheFirstEntryAndKeepsEveryKey`
- `internal/jobs/scheduler.TestSynchronizeFirstPartyRuntimesFromCleanArtifactStore`
- `internal/jobs/scheduler.TestTheAPICheckKeysOnTheReleaseBaselineNotTheCommit`
- `internal/jobs/scheduler.TestTheJobContextCarriesTheTagsTheCacheKeyHolds`
- `internal/jobs/scheduler.TestTwoLanesOnOneTreeShareTheirCacheKeys`
- `internal/jobs/scheduler.TestTypeScriptGenerateEditOutsideItsKeyExecutesAtTheNewBinding`
- `internal/jobs/scheduler.TestWarmSourceBoundHitsComputeTheBindingOnce`
- `internal/jobs/toolchain.TestACheckOfAMovedBinaryRecordsNothing`
- `internal/jobs/toolchain.TestACwdSelectedVersionThatDiffersDuringTheCheckIsNotRecorded`
- `internal/jobs/toolchain.TestAPassthroughDispatcherIsNotRecorded`
- `internal/jobs/toolchain.TestARelativeSelfAnswerNamesNoFile`
- `internal/jobs/toolchain.TestFirstPartyBunRuntimesResolveTheInstallUnderThePutnamiHome`
- `internal/jobs/toolchain.TestFirstPartyUVRuntimeResolvesTheInstallUnderThePutnamiHome`
- `internal/jobs/toolchain.TestFirstPartyWorkspaceInstallRunsBeforeTheLockPinsItsToolchain`
- `internal/jobs/toolchain.TestTheClientgenEmitterResolvesAnInstallThatHoldsBinBun`
- `internal/telemetry.TestCommandVocabularyMatchesFirstPartyCommands`
- `internal/watch.TestServeFixtureAnswersTheAdvertisedProtocolVersion`
- `internal/watch.TestServeLoop_TypedReadinessFromARealJobArmsAndReArms`
- `internal/workspace.TestCheckedInGeneratedClientsCarryAContractEdge`
- `internal/workspace.TestCheckedInNPMPublishProjectsCarryLocalTypeScriptConfig`
- `internal/workspace.TestCheckedInNPMWorkspaceProjectionMatchesMembership`
- `internal/workspace.TestCheckedInProtocolProjectsCarryProtocolTag`
- `internal/workspace.TestCheckedInWorkspaceClassifiesEveryPublicSubject`
<!-- marked-tests:end -->

## Decisions

[ADR 0001](doc/adr/0001-the-cli-tests-of-this-repository-run-from-a-dependent.md) records the marker, the root helpers that fail, and the proofs this project declares for the marked tests.

## Coverage

The package carries no production code, so coverage is off here.
