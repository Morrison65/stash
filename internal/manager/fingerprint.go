package manager

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/stashapp/stash/internal/manager/config"
	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/hash/md5"
	"github.com/stashapp/stash/pkg/hash/oshash"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
)

type fingerprintCalculator struct {
	Config *config.Config
}

func (c *fingerprintCalculator) calculateOshash(f *models.BaseFile, o file.Opener) (*models.Fingerprint, error) {
	r, err := o.Open()
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}

	defer r.Close()

	rc, isRC := r.(io.ReadSeeker)
	if !isRC {
		return nil, errors.New("cannot calculate oshash for non-readcloser")
	}

	hash, err := oshash.FromReader(rc, f.Size)
	if err != nil {
		return nil, fmt.Errorf("calculating oshash: %w", err)
	}

	return &models.Fingerprint{
		Type:        models.FingerprintTypeOshash,
		Fingerprint: hash,
	}, nil
}

func (c *fingerprintCalculator) calculateMD5(o file.Opener) (*models.Fingerprint, error) {
	r, err := o.Open()
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}

	defer r.Close()

	hash, err := md5.FromReader(r)
	if err != nil {
		return nil, fmt.Errorf("calculating md5: %w", err)
	}

	return &models.Fingerprint{
		Type:        models.FingerprintTypeMD5,
		Fingerprint: hash,
	}, nil
}

// remoteFingerprints computes the requested fingerprints on the file system
// that holds the file (stash-fsd), avoiding streaming the whole file for md5.
// It returns ok=false when the file system does not support it.
func remoteFingerprints(o file.Opener, wantMD5, wantOshash bool) (md5Hash, oshashHash string, ok bool, err error) {
	po, isPathOpener := o.(file.PathOpener)
	if !isPathOpener {
		return "", "", false, nil
	}

	fp, isFingerprinter := po.FS().(file.Fingerprinter)
	if !isFingerprinter {
		return "", "", false, nil
	}

	// a routing file system only fingerprints remotely for library paths
	if s, ok := fp.(interface{ SupportsFingerprint(string) bool }); ok && !s.SupportsFingerprint(po.Path()) {
		return "", "", false, nil
	}

	res, err := fp.Fingerprint(context.Background(), po.Path(), "", wantMD5, wantOshash)
	if err != nil {
		return "", "", true, fmt.Errorf("calculating fingerprints remotely: %w", err)
	}

	return res.MD5, res.Oshash, true, nil
}

func (c *fingerprintCalculator) CalculateFingerprints(f *models.BaseFile, o file.Opener, useExisting bool) ([]models.Fingerprint, error) {
	var existingOshash, existingMD5 *models.Fingerprint
	if useExisting {
		existingOshash = f.Fingerprints.For(models.FingerprintTypeOshash)
		existingMD5 = f.Fingerprints.For(models.FingerprintTypeMD5)
	}

	isVideo := useAsVideo(f.Path)
	wantOshash := isVideo && existingOshash == nil
	// only calculate MD5 for videos if enabled in config
	useMD5 := !isVideo || c.Config.IsCalculateMD5()
	wantMD5 := useMD5 && existingMD5 == nil

	if wantMD5 && useExisting {
		// log to indicate missing fingerprint is being calculated
		logger.Infof("Calculating checksum for %s ...", f.Path)
	}

	var oshashFP, md5FP *models.Fingerprint

	if wantOshash || wantMD5 {
		md5Hash, oshashHash, remote, err := remoteFingerprints(o, wantMD5, wantOshash)
		if err != nil {
			return nil, err
		}

		if remote {
			if wantOshash {
				oshashFP = &models.Fingerprint{Type: models.FingerprintTypeOshash, Fingerprint: oshashHash}
			}
			if wantMD5 {
				md5FP = &models.Fingerprint{Type: models.FingerprintTypeMD5, Fingerprint: md5Hash}
			}
		} else {
			if wantOshash {
				if oshashFP, err = c.calculateOshash(f, o); err != nil {
					return nil, err
				}
			}
			if wantMD5 {
				if md5FP, err = c.calculateMD5(o); err != nil {
					return nil, err
				}
			}
		}
	}

	var ret []models.Fingerprint

	if isVideo {
		if oshashFP == nil {
			oshashFP = existingOshash
		}
		ret = append(ret, *oshashFP)
	}

	if useMD5 {
		if md5FP == nil {
			md5FP = existingMD5
		}
		ret = append(ret, *md5FP)
	}

	return ret, nil
}
