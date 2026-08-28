terraform {
  required_version = ">= 1.10.0"

  # BYO state. The fixture uses the local backend so it runs with no cloud
  # credentials; Caesium never masters state (design §7).
  backend "local" {
    path = "terraform.tfstate"
  }

  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
