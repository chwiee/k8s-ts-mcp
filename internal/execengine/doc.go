// Package execengine runs a playbook's escalation ladder: up to three
// distinct actions, each with a pre-action state snapshot, a post-action
// healthcheck, and its own rollback.
package execengine
