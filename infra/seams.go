package infra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/dmitriyb/faber/config"
)

// This file adapts ImageBuilder to the config module's CLI seams
// (config.PackageProver, config.ImageBuilder), so integration wiring is one
// constructor call per capability. The adapters iterate templates in sorted
// order and join errors, matching validate's report-everything discipline.

// PackageProver adapts the builder's per-template resolution proof to the
// config.PackageProver seam consumed by faber validate.
func (b *ImageBuilder) PackageProver() config.PackageProver {
	return proverSeam{b: b}
}

type proverSeam struct {
	b *ImageBuilder
}

func (p proverSeam) ProvePackages(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(cfg.Templates)) {
		if err := p.b.ProvePackages(ctx, name, cfg.Templates[name].Build); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ConfigBuilder adapts Build to the config.ImageBuilder seam consumed by
// faber build.
func (b *ImageBuilder) ConfigBuilder() config.ImageBuilder {
	return builderSeam{b: b}
}

type builderSeam struct {
	b *ImageBuilder
}

func (s builderSeam) BuildImage(ctx context.Context, cfg *config.Config, template string, logger *slog.Logger) error {
	tpl, ok := cfg.Templates[template]
	if !ok {
		return fmt.Errorf("infra: unknown template %q", template)
	}
	tag, err := s.b.Build(ctx, template, tpl.Build)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "image ready", "template", template, "tag", tag)
	return nil
}
