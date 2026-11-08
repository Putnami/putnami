rootProject.name = "java-sample-gradle"

dependencyResolutionManagement {
    repositories {
        mavenCentral()
    }
}

include(":library", ":application")
