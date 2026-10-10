// Package ci defines the source-controlled Putnami workspace CI intent
// contract, the immutable ChangePlan document `putnami change-plan` emits for
// CI admission, and the ImpactPlan document `putnami impact-plan` emits for an
// extension, which ChangePlanFromImpactPlan projects onto a ChangePlan. It is
// deliberately plane-neutral: the framework validates and canonicalizes
// authored intent and admission plans while Cloud supplies trust and execution
// authority.
package ci
