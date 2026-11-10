# ADR 0005 — An Android build gets the SDK the workspace accepts

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `@putnami/java`, its Android variants and its
  `workspace-install`
- **Amends**: [ADR 0004](0004-a-blank-machine-builds-after-putnami-install.md)

## Context

[ADR 0004](0004-a-blank-machine-builds-after-putnami-install.md) lets a blank
machine build a Gradle or Maven build after `putnami install`, and leaves the
Android SDK out. An Android build reads the SDK from `ANDROID_HOME`,
`ANDROID_SDK_ROOT` or `sdk.dir` in `local.properties`; without one, Gradle
fails to configure the build with "SDK location not found", for every task of
the build, the projects without the Android plugin included.

Installing the SDK accepts its licenses. The Android Gradle plugin downloads
the platforms and build tools a build declares only into an SDK whose licenses
are accepted. Accepting them on the team's behalf is a decision of the
workspace's owner, so the extension cannot take it by default.

## Decision

- **The opt-in.** The workspace's owner accepts the licenses in
  `putnami.workspace.json`:
  `"options": {"@putnami/java": {"android": {"acceptSdkLicenses": true}}}`.
  The option reaches the extension through the job context
  ([ADR 0016 of the stack core](../../../../tooling/stacks/doc/adr/0016-a-stack-sets-the-environment-of-a-task.md)).
- **What the extension installs.** With the option, a task of a Gradle build
  that applies the Android plugin, and the `workspace-install` of its stack
  root, install the Android SDK command-line tools 23.0 (build 16111833) under
  `toolchains/android` of the Putnami home, checked by the SHA-256 of the
  archive for Linux, macOS on x64 and arm64, and Windows, and accept the
  licenses with `sdkmanager --licenses`, run on the pinned JDK 21. The SDK is
  `toolchains/android/sdk`, shared by the machine's builds; the tasks get it as
  `ANDROID_HOME` and `ANDROID_SDK_ROOT`. The Android Gradle plugin then
  downloads the platforms and build tools the build declares.
- **The machine's SDK first.** `ANDROID_HOME`, `ANDROID_SDK_ROOT` or `sdk.dir`
  in the stack root's `local.properties` keeps the machine's SDK, with or
  without the option.
- **Without the option.** A task of an Android component with no SDK fails
  with `stacks.toolchain_missing`, and the message names the option. A task of
  another component of an Android build is left to Gradle.
- **Offline.** A run installs nothing: it uses the SDK an earlier run or
  `putnami install` installed, and an Android component with none fails.
- **What makes a build Android.** The component's variant, `android` or
  `android-coverage`, or a build file of the stack root or of one of its
  projects that applies the Android plugin, as the detector reads it.

## Rejected alternatives

- **Write the license files.** Writing the license hashes into
  `sdk/licenses` accepts licenses no one read, and breaks when Google changes
  the text; `sdkmanager --licenses` accepts the licenses the tools state.
- **Install the platforms and build tools ahead.** It means reading
  `compileSdk` and the build tools version from build scripts that compute
  them; the Android Gradle plugin reads them and downloads what it needs.
- **A project option.** The licenses bind the team, not one project, and every
  project of the workspace shares the machine's SDK.

## Consequences

- A blank machine builds an Android project after `putnami install`, once the
  workspace's owner set the option.
- The SDK's platforms and build tools download on the first task that needs
  them, or during `putnami install`, whose Gradle resolution configures the
  build.
- Linux on arm64 gets no command-line tools: Google publishes the build tools
  for x64 only.
- A new release of the command-line tools is a change of the extension, with
  its SHA-256.
