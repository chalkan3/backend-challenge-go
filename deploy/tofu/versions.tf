// Pin the provider and the OpenTofu floor.
//
// The lock file (.terraform.lock.hcl) is committed so that `tofu init` resolves
// the same provider build on every machine and in CI — an unpinned provider is
// a supply-chain hole and a source of "works here, fails there".
//
// WHY >= 6.0 AND NOT 5.x
// ----------------------
// Measured, not guessed. hashicorp/aws v5 validates max_message_size against
// the range (1024 - 262144) and REFUSES the 1 MiB that SQS itself accepts:
//
//   aws v5.100.0: Error: expected max_message_size to be in the range
//                        (1024 - 262144), got 1048576
//   aws v6.68.0: applies it; the queue comes back as MaximumMessageSize=1048576
//
// The older provider would have silently provisioned a 256 KiB queue — smaller
// than the 1 MiB request body the API itself accepts (see the
// max_message_size_bytes variable). That is a data-path regression disguised as
// a defaults question, so the constraint is v6.
terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}
