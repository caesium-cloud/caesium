terraform {
  required_version = ">= 1.10.0"

  backend "local" {
    path = "terraform.tfstate"
  }

  required_providers {
    # local is the only provider in this fixture whose Read consults the real
    # world: local_file checks that the file still exists and still hashes to
    # what state records, and drops the resource from state when it does not.
    #
    # That is the whole reason this stack exists. null and random — the
    # providers every stack under stacks/ uses — are state-only, so their
    # refresh is a no-op and `plan -refresh-only` can NEVER report drift for
    # them, no matter how the state is edited. Without a resource like this one
    # the drift scenario (design §6.6, §3.2) would be untestable and the drift
    # job would be shipped unverified.
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }
}
