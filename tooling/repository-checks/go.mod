// The repository checks are their own module so that their test task depends
// on no other project: the module requires nothing, and reads every file it
// checks as committed text. Nothing publishes this module.
module go.putnami.dev/tooling/repositorychecks

go 1.25.7
