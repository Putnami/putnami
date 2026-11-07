/*
 * The Structurizr "Big Bank plc" example, adapted to a fixture repository:
 * the elements that have code carry a `path` property naming a project.
 */
workspace "Big Bank plc" "A fictional retail bank, adapted for Putnami." {

    !identifiers flat

    model {
        customer = person "Personal Banking Customer" "A customer of the bank." "Customer"

        group "Big Bank plc" {
            supportStaff = person "Customer Service Staff" "Staff in customer service." "Bank Staff"
            backoffice = person "Back Office Staff" "Administration and support staff." "Bank Staff"

            mainframe = softwaresystem "Mainframe Banking System" "Stores customers, accounts and transactions." "Existing System" {
                properties {
                    "path" "mainframe"
                }
            }
            email = softwaresystem "E-mail System" "The internal e-mail system." "Existing System" {
                properties {
                    path email
                }
            }
            atm = softwaresystem "ATM" "Lets customers withdraw cash." "Existing System"

            internetBankingSystem = softwaresystem "Internet Banking System" "Lets customers view their accounts and make payments." {
                singlePageApplication = container "Single-Page Application" "The banking functions in the browser." "JavaScript and Angular" "Web Browser" {
                    properties {
                        "path" "spa"
                    }
                }
                mobileApp = container "Mobile App" "A subset of the banking functions on a phone." "Xamarin" "Mobile App"
                webApplication = container "Web Application" "Delivers the static content and the single-page application." "Java and Spring MVC" {
                    properties {
                        "path" "web-application"
                    }
                }
                apiApplication = container "API Application" "Banking functions over a JSON/HTTPS API." "Java and Spring MVC" {
                    properties {
                        "path" "api"
                    }
                    signinController = component "Sign In Controller" "Signs users in." "Spring MVC Rest Controller"
                    accountsSummaryController = component "Accounts Summary Controller" "Lists the accounts of a customer." "Spring MVC Rest Controller"
                    resetPasswordController = component "Reset Password Controller" "Resets a password with a single-use URL." "Spring MVC Rest Controller"
                    securityComponent = component "Security Component" "Signs in and changes passwords." "Spring Bean"
                    mainframeBankingSystemFacade = component "Mainframe Banking System Facade" "Facade onto the mainframe." "Spring Bean" {
                        properties {
                            "path" "api/mainframe-facade"
                        }
                    }
                    emailComponent = component "E-mail Component" "Sends e-mails." "Spring Bean" {
                        properties {
                            "path" "api/email-component"
                        }
                    }
                }
                database = container "Database" "Stores credentials and access logs." "Oracle Database Schema" "Database"
            }
        }

        // People and software systems
        customer -> internetBankingSystem "Views account balances and makes payments using"
        internetBankingSystem -> mainframe "Gets account information from, and makes payments using"
        internetBankingSystem -> email "Sends e-mail using"
        email -> customer "Sends e-mails to"
        customer -> supportStaff "Asks questions to" "Telephone"
        supportStaff -> mainframe "Uses"
        customer -> atm "Withdraws cash using"
        atm -> mainframe "Uses"
        backoffice -> mainframe "Uses"

        // Containers
        customer -> webApplication "Visits bigbank.com/ib using" "HTTPS"
        customer -> singlePageApplication "Views account balances and makes payments using"
        customer -> mobileApp "Views account balances and makes payments using"
        webApplication -> singlePageApplication "Delivers to the customer's web browser"

        // Components
        singlePageApplication -> signinController "Makes API calls to" "JSON/HTTPS"
        singlePageApplication -> accountsSummaryController "Makes API calls to" "JSON/HTTPS"
        singlePageApplication -> resetPasswordController "Makes API calls to" "JSON/HTTPS"
        mobileApp -> signinController "Makes API calls to" "JSON/HTTPS"
        mobileApp -> accountsSummaryController "Makes API calls to" "JSON/HTTPS"
        signinController -> securityComponent "Uses"
        accountsSummaryController -> mainframeBankingSystemFacade "Uses"
        resetPasswordController -> securityComponent "Uses"
        resetPasswordController -> emailComponent "Uses"
        securityComponent -> database "Reads from and writes to" "SQL/TCP"
        mainframeBankingSystemFacade -> mainframe "Makes API calls to" "XML/HTTPS"
        emailComponent -> email "Sends e-mail using" "SMTP"

        deploymentEnvironment "Live" {
            deploymentNode "Big Bank plc" "" "Big Bank plc data center" {
                deploymentNode "bigbank-api***" "" "Ubuntu 16.04 LTS" "" 8 {
                    containerInstance apiApplication
                }
            }
        }
    }

    views {
        systemlandscape "SystemLandscape" {
            include *
            autoLayout
        }
        styles {
            element "Person" {
                color #ffffff
                shape Person
            }
        }
    }
}
