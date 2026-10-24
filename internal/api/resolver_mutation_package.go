package api

import (
	"context"
	"strconv"

	"github.com/stashapp/stash/internal/manager"
	"github.com/stashapp/stash/internal/manager/task"
	"github.com/stashapp/stash/pkg/job"
	"github.com/stashapp/stash/pkg/models"
)

func refreshPackageType(typeArg PackageType) {
	mgr := manager.GetInstance()

	if typeArg == PackageTypePlugin {
		mgr.RefreshPluginCache()
	} else if typeArg == PackageTypeScraper {
		mgr.RefreshScraperCache()
	}
}

func (r *mutationResolver) InstallPackages(ctx context.Context, typeArg PackageType, packages []*models.PackageSpecInput) (string, error) {
	pm, err := getPackageManager(typeArg)
	if err != nil {
		return "", err
	}

	mgr := manager.GetInstance()
	t := &task.InstallPackagesJob{
		PackagesJob: task.PackagesJob{
			PackageManager: pm,
			OnComplete:     func() { refreshPackageType(typeArg) },
		},
		Packages: packages,
	}
	jobID := mgr.JobManager.AddInLane(ctx, job.LaneSystem, "Installing packages...", t)

	return strconv.Itoa(jobID), nil
}

func (r *mutationResolver) UpdatePackages(ctx context.Context, typeArg PackageType, packages []*models.PackageSpecInput) (string, error) {
	pm, err := getPackageManager(typeArg)
	if err != nil {
		return "", err
	}

	mgr := manager.GetInstance()
	t := &task.UpdatePackagesJob{
		PackagesJob: task.PackagesJob{
			PackageManager: pm,
			OnComplete:     func() { refreshPackageType(typeArg) },
		},
		Packages: packages,
	}
	jobID := mgr.JobManager.AddInLane(ctx, job.LaneSystem, "Updating packages...", t)

	return strconv.Itoa(jobID), nil
}

func (r *mutationResolver) UninstallPackages(ctx context.Context, typeArg PackageType, packages []*models.PackageSpecInput) (string, error) {
	pm, err := getPackageManager(typeArg)
	if err != nil {
		return "", err
	}

	mgr := manager.GetInstance()
	t := &task.UninstallPackagesJob{
		PackagesJob: task.PackagesJob{
			PackageManager: pm,
			OnComplete:     func() { refreshPackageType(typeArg) },
		},
		Packages: packages,
	}
	jobID := mgr.JobManager.AddInLane(ctx, job.LaneSystem, "Updating packages...", t)

	return strconv.Itoa(jobID), nil
}
