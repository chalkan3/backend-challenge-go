// The AWS provider, pointed at whichever SQS this run is about.
//
// The skip_* flags and the static "test"/"test" credentials are only meaningful
// when talking to LocalStack, which does not validate either. They are inert
// against real AWS, where credentials come from the environment (or an instance
// profile) and the account id is real. Keeping them here — rather than a second
// provider block — is what lets one declaration serve both targets.
//
// Note that credentials validation cannot be "off" against AWS: if you point
// this at a real account, remove them and let the SDK resolve the chain.
provider "aws" {
  region = var.region

  access_key = "test"
  secret_key = "test"

  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true

  // LocalStack's SQS addresses buckets-style paths; harmless on AWS.
  s3_use_path_style = true

  endpoints {
    // Empty string falls back to the provider default (real AWS).
    sqs = var.sqs_endpoint != "" ? var.sqs_endpoint : null
  }
}
