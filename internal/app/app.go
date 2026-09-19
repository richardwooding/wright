// Package app is the composition root: it will wire config → model → tools →
// policy → engine → tui | headless. In this phase it only hosts the
// architecture tests (import DAG, no unexpected network) that guard the
// package boundaries while the rest of the tree is built.
package app
