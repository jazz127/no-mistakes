package config

import "fmt"

func validateRequiredReviews(reviewers []string) error {
	seen := map[string]bool{}
	for _, reviewer := range reviewers {
		if reviewer != "coderabbit" {
			return fmt.Errorf("ci.required_reviews: unsupported reviewer %q (supported: coderabbit)", reviewer)
		}
		if seen[reviewer] {
			return fmt.Errorf("ci.required_reviews: duplicate reviewer %q", reviewer)
		}
		seen[reviewer] = true
	}
	return nil
}
