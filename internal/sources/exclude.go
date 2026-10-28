package sources

import "samuori.invalid/samuori/internal/excludes"

func DefaultExcludes() []string { return excludes.Defaults() }

func KeptFiles() []string { return excludes.KeptFiles() }

func SecretFilePatterns() []string { return excludes.SecretFilePatterns() }

func ValidatePatterns(patterns []string) error { return excludes.Validate(patterns) }

type excluder = excludes.Matcher

func newExcluder(user []string) excluder { return excludes.New(user) }
