// Package ci defines the source-controlled Putnami workspace CI intent
// contract, the ImpactPlan document `putnami impact-plan` emits for an
// extension, and the immutable ChangePlan admission document a consumer
// derives from it with ChangePlanFromImpactPlan. It is deliberately
// plane-neutral: the framework validates and canonicalizes authored intent and
// admission plans while Cloud supplies trust and execution authority.
package ci
