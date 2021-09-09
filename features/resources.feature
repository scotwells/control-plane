Feature: Resource CRUD Management
  In order to manage resources
  As a user of the system
  I need to be able to manage the resources that are created in the system

  Scenario: Successfully CRUD a registered resource
    Given the resource "features.Account" is registered
     When creating the following resource:
      """
        {
          "resource": {
            "@type": "features.Account",
            "display_name": "My Testing Account",
            "name": "accounts/default-account"
          }
        }
      """
     Then I will receive a successful response
      And the response value "displayName" will be "My Testing Account"
     When getting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
     Then I will receive a successful response
      And the response value "displayName" will be "My Testing Account"
     When updating the following resource:
       """
        {
          "resource": {
            "@type": "features.Account",
            "name": "accounts/default-account",
            "display_name": "Updated Account Name"
          }
        }
       """
     Then I will receive a successful response
      And the response value "displayName" will be "Updated Account Name"
      And the response value "updateTime" will be within "1s" from now
     When getting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
     Then I will receive a successful response
      And the response value "displayName" will be "Updated Account Name"
     When deleting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
     Then I will receive a successful response
      And the response value "deleteTime" will be within "1s" from now
     When getting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
     Then I will receive a successful response
      And the response value "deleteTime" will be within "1s" from now

  Scenario: Successfully undelete a resource that has been deleted
    Given the resource "features.Account" is registered
      And creating the following resource:
       """
        {
          "resource": {
            "@type": "features.Account",
            "display_name": "My Testing Account",
            "name": "accounts/default-account"
          }
        }
       """
      And deleting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
     When undeleting the following resource
      """
       {
         "resource_type": "features.Account",
         "name": "accounts/default-account"
       }
      """
     Then I will receive a successful response
      And getting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
     Then I will receive a successful response

  Scenario: Successfully purge a resource that has been deleted
    Given the resource "features.Account" is registered
      And creating the following resource:
       """
        {
          "resource": {
            "@type": "features.Account",
            "display_name": "My Testing Account",
            "name": "accounts/default-account"
          }
        }
       """
      And deleting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
     When purging the following resource
      """
       {
         "resource_type": "features.Account",
         "name": "accounts/default-account"
       }
      """
     Then I will receive a successful response
      And getting the following resource:
       """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
       """
      And I will receive an error with code "NOT_FOUND"


  Scenario: NotFound error when resource doesn't exist
    Given the resource "features.Account" is registered
     When getting the following resource:
      """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
      """
     Then I will receive an error with code "NOT_FOUND"
     When updating the following resource:
      """
        {
          "resource": {
            "@type": "features.Account",
            "name": "accounts/default-account",
            "display_name": "My Account #2"
          }
        }
      """
     Then I will receive an error with code "NOT_FOUND"
     When deleting the following resource:
      """
        {
          "resource_type": "features.Account",
          "name": "accounts/default-account"
        }
      """
     Then I will receive an error with code "NOT_FOUND"

  Scenario: Error on Unregisterd Resources
   Given no resources are registered
    When creating the following resource:
      """
        {
          "resource": {
            "@type": "features.Account",
            "display_name": "My Testing Account",
            "name": "accounts/default-account"
          }
        }
      """
     Then I will receive an error with code "UNIMPLEMENTED"
    When getting the following resource:
      """
        {
          "resourceType": "features.Account",
          "name": "accounts/default-account"
        }
      """
     Then I will receive an error with code "UNIMPLEMENTED"
    When updating the following resource:
      """
        {
          "resource": {
            "@type": "features.Account",
            "display_name": "My Testing Account #2",
            "name": "accounts/default-account"
          }
        }
      """
     Then I will receive an error with code "UNIMPLEMENTED"
    When deleting the following resource:
      """
        {
          "resourceType": "features.Account",
          "name": "accounts/default-account"
        }
      """
     Then I will receive an error with code "UNIMPLEMENTED"
