package extension

// UnsafeInstallEnv lets users opt in to installing an archive whose
// authenticity cannot be verified (no lockfile entry and no integrity
// header from the resolver). It is intended for the transition period
// before resolvers advertise integrity hashes; routine use defeats the
// only defense against a tampered download.
const UnsafeInstallEnv = "PUTNAMI_UNSAFE_INSTALL"
