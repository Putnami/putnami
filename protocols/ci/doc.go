// Package ci defines the source-controlled Putnami workspace CI intent
// contract and the immutable ChangePlan document `putnami change-plan` emits
// for CI admission. It is deliberately plane-neutral: the framework validates
// and canonicalizes authored intent and admission plans while Cloud supplies
// trust and execution authority.
package ci
