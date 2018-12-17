package selfupdate

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/blang/semver"
	"github.com/google/go-github/github"
)

// ReleaseType defines what kind of releases caller wants to upgrade to
type ReleaseType uint8

const (
	// RELEASE restricts versions to proper semver releases
	RELEASE ReleaseType = 1 << iota

	// PRERELEASE restricts version to proper semver pre-releases (not releases)
	PRERELEASE
)

// IsAllowed returns true if given ReleaseType is enabled in given mask
func (f ReleaseType) IsAllowed(flag ReleaseType) bool { return f&flag != 0 }

func findAssetFromRelease(rel *github.RepositoryRelease, suffixes []string, targetVersion string, releaseTypes ReleaseType) (*github.ReleaseAsset, semver.Version, bool) {
	if targetVersion != "" && targetVersion != rel.GetTagName() {
		log.Println("Skip", rel.GetTagName(), "not matching to specified version", targetVersion)
		return nil, semver.Version{}, false
	}

	if targetVersion == "" && rel.GetDraft() {
		log.Println("Skip draft version", rel.GetTagName())
		return nil, semver.Version{}, false
	} else if targetVersion == "" && rel.GetPrerelease() && !releaseTypes.IsAllowed(PRERELEASE) {
		log.Println("Skip pre-release version", rel.GetTagName())
		return nil, semver.Version{}, false
	} else if !releaseTypes.IsAllowed(RELEASE) {
		log.Println("Skip release version", rel.GetTagName())
		return nil, semver.Version{}, false
	}

	verText := strings.TrimPrefix(rel.GetTagName(), "v")

	// If semver cannot parse the version text, it means that the text is not adopting
	// the semantic versioning. So it should be skipped.
	ver, err := semver.Make(verText)
	if err != nil {
		log.Println("Failed to parse a semantic version", verText)
		return nil, semver.Version{}, false
	}

	for _, asset := range rel.Assets {
		name := asset.GetName()
		for _, s := range suffixes {
			if strings.HasSuffix(name, s) {
				return &asset, ver, true
			}
		}
	}

	log.Println("No suitable asset was found in release", rel.GetTagName())
	return nil, semver.Version{}, false
}

func findValidationAsset(rel *github.RepositoryRelease, validationName string) (*github.ReleaseAsset, bool) {
	for _, asset := range rel.Assets {
		if asset.GetName() == validationName {
			return &asset, true
		}
	}
	return nil, false
}

func findReleaseAndAsset(rels []*github.RepositoryRelease, targetVersion string, releaseTypes ReleaseType) (*github.RepositoryRelease, *github.ReleaseAsset, semver.Version, bool) {
	// Generate candidates
	suffixes := make([]string, 0, 2*7*2)
	for _, sep := range []rune{'_', '-'} {
		for _, ext := range []string{".zip", ".tar.gz", ".gzip", ".gz", ".tar.xz", ".xz", ""} {
			suffix := fmt.Sprintf("%s%c%s%s", runtime.GOOS, sep, runtime.GOARCH, ext)
			suffixes = append(suffixes, suffix)
			if runtime.GOOS == "windows" {
				suffix = fmt.Sprintf("%s%c%s.exe%s", runtime.GOOS, sep, runtime.GOARCH, ext)
				suffixes = append(suffixes, suffix)
			}
		}
	}

	var ver semver.Version
	var asset *github.ReleaseAsset
	var release *github.RepositoryRelease

	// Find the latest version from the list of releases.
	// Returned list from GitHub API is in the order of the date when created.
	//   ref: https://github.com/rhysd/go-github-selfupdate/issues/11
	for _, rel := range rels {
		if a, v, ok := findAssetFromRelease(rel, suffixes, targetVersion, releaseTypes); ok {
			// Note: any version with suffix is less than any version without suffix.
			// e.g. 0.0.1 > 0.0.1-beta
			fmt.Println(v)
			if release == nil || v.GTE(ver) {
				ver = v
				asset = a
				release = rel
			}
		}
	}

	if release == nil {
		log.Println("Could not find any release for", runtime.GOOS, "and", runtime.GOARCH)
		return nil, nil, semver.Version{}, false
	}

	return release, asset, ver, true
}

func (up *Updater) detectVersion(slug string, version string, releaseTypes ReleaseType) (release *Release, found bool, err error) {

	repo := strings.Split(slug, "/")
	if len(repo) != 2 || repo[0] == "" || repo[1] == "" {
		return nil, false, fmt.Errorf("Invalid slug format. It should be 'owner/name': %s", slug)
	}

	rels, res, err := up.api.Repositories.ListReleases(up.apiCtx, repo[0], repo[1], nil)
	if err != nil {
		log.Println("API returned an error response:", err)
		if res != nil && res.StatusCode == 404 {
			// 404 means repository not found or release not found. It's not an error here.
			err = nil
			log.Println("API returned 404. Repository or release not found")
		}
		return nil, false, err
	}

	rel, asset, ver, found := findReleaseAndAsset(rels, version, releaseTypes)
	if !found {
		return nil, false, nil
	}

	url := asset.GetBrowserDownloadURL()
	log.Println("Successfully fetched the latest release. tag:", rel.GetTagName(), ", name:", rel.GetName(), ", URL:", rel.GetURL(), ", Asset:", url)

	publishedAt := rel.GetPublishedAt().Time
	release = &Release{
		ver,
		url,
		asset.GetSize(),
		asset.GetID(),
		-1,
		rel.GetHTMLURL(),
		rel.GetBody(),
		rel.GetName(),
		&publishedAt,
		repo[0],
		repo[1],
	}

	if up.validator != nil {
		validationName := asset.GetName() + up.validator.Suffix()
		validationAsset, ok := findValidationAsset(rel, validationName)
		if !ok {
			return nil, false, fmt.Errorf("Failed finding validation file %q", validationName)
		}
		release.ValidationAssetID = validationAsset.GetID()
	}

	return release, true, nil
}

// DetectLatest tries to get the latest version of the repository on GitHub. 'slug' means 'owner/name' formatted string.
// It fetches releases information from GitHub API and find out the latest release with matching the tag names and asset names.
// Drafts and pre-releases are ignored. Assets would be suffixed by the OS name and the arch name such as 'foo_linux_amd64'
// where 'foo' is a command name. '-' can also be used as a separator. File can be compressed with zip, gzip, zxip, tar&zip or tar&zxip.
// So the asset can have a file extension for the corresponding compression format such as '.zip'.
// On Windows, '.exe' also can be contained such as 'foo_windows_amd64.exe.zip'.
func (up *Updater) DetectLatest(slug string) (release *Release, found bool, err error) {
	return up.DetectVersion(slug, "")
}

// DetectVersion tries to get the given version of the repository on Github. `slug` means `owner/name` formatted string.
// And version indicates the required version.
func (up *Updater) DetectVersion(slug string, version string) (release *Release, found bool, err error) {
	return up.detectVersion(slug, version, RELEASE)
}

// DetectLatestOfType tries to get the latest version of the repository on Github. `slug` means `owner/name` formatted string.
// ReleaseType defines allowed release types, such as RELEASE, PRERELEASE or DRAFT.
// These can be combined like bit masks: RELEASE | PRERELEASE or PRERELEASE | DRAFT
func (up *Updater) DetectLatestOfType(slug string, releaseTypes ReleaseType) (release *Release, found bool, err error) {
	return up.detectVersion(slug, "", releaseTypes)
}

// DetectVersionOfType tries to get the given version of the repository on Github. `slug` means `owner/name` formatted string.
// And version indicates the required version. ReleaseType defines allowed release types, such as RELEASE, PRERELEASE or DRAFT.
// These can be combined like bit masks: RELEASE | PRERELEASE or PRERELEASE | DRAFT
func (up *Updater) DetectVersionOfType(slug string, version string, releaseTypes ReleaseType) (release *Release, found bool, err error) {
	return up.detectVersion(slug, version, releaseTypes)
}

// DetectLatest detects the latest release of the slug (owner/repo).
// This function is a shortcut version of updater.DetectLatest() method.
func DetectLatest(slug string) (*Release, bool, error) {
	return DefaultUpdater().DetectLatest(slug)
}

// DetectLatestOfType detects the latest release of the slug (owner/repo) with given release types (RELEASE, PRERELEASE).
// This function is a shortcut version of updater.DetectLatestOfType() method.
func DetectLatestOfType(slug string, version string, releaseTypes ReleaseType) (*Release, bool, error) {
	return DefaultUpdater().DetectLatestOfType("", releaseTypes)
}

// DetectVersion detects the given release of the slug (owner/repo) from its version.
func DetectVersion(slug string, version string) (*Release, bool, error) {
	return DefaultUpdater().DetectVersion(slug, version)
}

// DetectVersionOfType detects the given release of the slug (owner/repo) with given release types (RELEASE, PRERELEASE).
// This function is a shortcut version of updater.DetectVersionOfType() method.
func DetectVersionOfType(slug string, version string, releaseTypes ReleaseType) (*Release, bool, error) {
	fmt.Println("Pling")
	return DefaultUpdater().DetectVersionOfType(slug, version, releaseTypes)
}
