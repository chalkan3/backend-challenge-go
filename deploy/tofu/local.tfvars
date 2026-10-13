// LocalStack is the default target: `tofu apply` with no arguments re-provisions
// the same queues the compose stack gets from 10-queues.py.
//
// This file exists so the local run is a single command with no -var plumbing.
// It hardcodes the endpoint only; everything else comes from variables.tf.
//
// Pointing at real AWS: pass -var sqs_endpoint= (empty) and let the credentials
// chain resolve, or use a separate .tfvars. Do NOT leave the static test/test
// credentials in play against a real account — see provider.tf.
sqs_endpoint = "http://localhost:4566"
