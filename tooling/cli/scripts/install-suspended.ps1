# Putnami CLI installer for Windows, served at https://putnami.dev/install.ps1
# while Windows support is suspended.
#
# Putnami builds, vets and publishes no Windows binary, so this script installs
# nothing and requests nothing: it says so and fails. The site sets the value of
# the one line below for ?run=<command>; the value changes nothing here.
#
# The Windows installer itself is tooling/cli/scripts/install.ps1 in the
# Putnami repository. It is kept, unverified, for when support comes back.

$RunCommandDefault = ''

throw 'Putnami does not support Windows for now: Windows support is suspended since 2026-11-26 and no Windows build is published, so there is nothing to install. Putnami runs on macOS and Linux, on amd64 or arm64: https://putnami.dev/install.sh'
