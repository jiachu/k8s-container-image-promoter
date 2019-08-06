/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package inventory

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	yaml "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	"github.com/google/go-containerregistry/pkg/v1/google"
	cr "github.com/google/go-containerregistry/pkg/v1/types"
	cipJson "sigs.k8s.io/k8s-container-image-promoter/lib/json"
	"sigs.k8s.io/k8s-container-image-promoter/lib/stream"
	"sigs.k8s.io/k8s-container-image-promoter/pkg/gcloud"
)

func getSrcRegistry(rcs []RegistryContext) (*RegistryContext, error) {
	for _, registry := range rcs {
		registry := registry
		if registry.Src {
			return &registry, nil
		}
	}
	return nil, fmt.Errorf("could not find source registry")
}

// MakeSyncContext creates a SyncContext.
func MakeSyncContext(
	manifestPath string,
	mfest Manifest,
	verbosity, threads int,
	dryRun, useSvcAcc bool) (SyncContext, error) {

	return SyncContext{
		Verbosity:           verbosity,
		Threads:             threads,
		ManifestPath:        manifestPath,
		DryRun:              dryRun,
		UseServiceAccount:   useSvcAcc,
		Inv:                 make(MasterInventory),
		InvIgnore:           []ImageName{},
		Tokens:              make(map[RootRepo]gcloud.Token),
		RenamesDenormalized: mfest.renamesDenormalized,
		SrcRegistry:         mfest.srcRegistry,
		RegistryContexts:    mfest.Registries,
		DigestMediaType:     make(DigestMediaType)}, nil
}

// ParseManifestFromFile parses a Manifest from a filepath.
func ParseManifestFromFile(
	filePath string) (Manifest, error) {

	var mfest Manifest
	var rd RenamesDenormalized
	var srcRegistry *RegistryContext
	b, err := ioutil.ReadFile(filePath)
	if err != nil {
		return mfest, err
	}

	mfest, err = ParseManifestYAML(b)
	if err != nil {
		return mfest, err
	}

	mfest.filepath = filePath

	// Perform semantic checks (beyond just YAML validation).
	if len(mfest.Registries) != 0 {
		srcRegistry, err = getSrcRegistry(mfest.Registries)
		if err != nil {
			return mfest, err
		}
		mfest.srcRegistry = srcRegistry

		rd, err = DenormalizeRenames(mfest, srcRegistry.Name)
		if err != nil {
			return mfest, err
		}
	}
	mfest.renamesDenormalized = rd

	return mfest, nil
}

// ParseManifestYAML parses a Manifest from a byteslice. This function is
// separate from ParseManifestFromFile() so that it can be tested independently.
func ParseManifestYAML(b []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.UnmarshalStrict(b, &m); err != nil {
		return m, err
	}

	return m, m.Validate()
}

// Validate checks for semantic errors in the yaml fields (the structure of the
// yaml is checked during unmarshaling).
func (m Manifest) Validate() error {
	if err := validateRequiredComponents(m); err != nil {
		return err
	}
	return validateImages(m.Images)
}

func validateImages(images []Image) error {
	for _, image := range images {
		for digest, tagSlice := range image.Dmap {
			if err := validateDigest(digest); err != nil {
				return err
			}

			for _, tag := range tagSlice {
				if err := validateTag(tag); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateDigest(digest Digest) error {
	validDigest := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	if !validDigest.Match([]byte(digest)) {
		return fmt.Errorf("invalid digest: %v", digest)
	}
	return nil
}

func validateTag(tag Tag) error {
	var validTag = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	if !validTag.Match([]byte(tag)) {
		return fmt.Errorf("invalid tag: %v", tag)
	}
	return nil
}

func validateRegistryImagePath(rip RegistryImagePath) error {
	validRegistryImagePath := regexp.MustCompile(
		// \w is [0-9a-zA-Z_]
		`^[\w-]+(\.[\w-]+)+(/[\w-]+)+$`)
	if !validRegistryImagePath.Match([]byte(rip)) {
		return fmt.Errorf("invalid registry image path: %v", rip)
	}
	return nil
}

func (m Manifest) srcRegistryCount() int {
	var count int
	for _, registry := range m.Registries {
		if registry.Src {
			count++
		}
	}
	return count
}

func (m Manifest) srcRegistryName() RegistryName {
	for _, registry := range m.Registries {
		if registry.Src {
			return registry.Name
		}
	}
	return RegistryName("")
}

// nolint[gocyclo]
func validateRequiredComponents(m Manifest) error {
	errs := make([]string, 0)
	srcRegistryName := RegistryName("")
	if len(m.Registries) > 0 {
		if m.srcRegistryCount() > 1 {
			errs = append(errs, fmt.Sprintf("cannot have more than 1 source registry"))
		}
		srcRegistryName = m.srcRegistryName()
		if len(srcRegistryName) == 0 {
			errs = append(errs, fmt.Sprintf("source registry must be set"))
		}
	}

	knownRegistries := make([]RegistryName, 0)
	if len(m.Registries) == 0 {
		errs = append(errs, fmt.Sprintf("'registries' field cannot be empty"))
	}
	for _, registry := range m.Registries {
		if len(registry.Name) == 0 {
			errs = append(
				errs,
				fmt.Sprintf("registries: 'name' field cannot be empty"))
		}
		knownRegistries = append(knownRegistries, registry.Name)
	}
	for _, image := range m.Images {
		if len(image.ImageName) == 0 {
			errs = append(
				errs,
				fmt.Sprintf("images: 'name' field cannot be empty"))
		}
		if len(image.Dmap) == 0 {
			errs = append(
				errs,
				fmt.Sprintf("images: 'dmap' field cannot be empty"))
		}
	}

	for _, rename := range m.Renames {
		// The names must be valid paths.
		// Each name must be the registry+pathname, *without* a trailing slash.
		if len(rename) < 2 {
			// nolint[lll]
			errs = append(errs,
				fmt.Sprintf("a rename entry must have at least 2 paths, one for the source, another for at least 1 dest registry"))
		}

		// Make sure there is only 1 rename per registry, and that there is 1
		// entry for the source registry..
		var srcOriginal RegistryImagePath
		seenRegistries := make(map[RegistryName]ImageName)
		for _, registryImagePath := range rename {
			// nolint[lll]
			registryName, imageName, err := SplitRegistryImagePath(registryImagePath, knownRegistries)
			if err != nil {
				errs = append(errs, err.Error())
			}

			if _, ok := seenRegistries[registryName]; ok {
				// nolint[lll]
				errs = append(errs, fmt.Sprintf("multiple renames found for registry '%v' in 'renames', for image %v",
					registryName,
					imageName))
			} else {
				seenRegistries[registryName] = imageName
			}

			if err = validateRegistryImagePath(registryImagePath); err != nil {
				errs = append(errs, err.Error())
			}

			// Check if the registry is found in the outer `registries` field.
			found := false
			for _, rc := range m.Registries {
				if rc.Name == registryName {
					found = true
				}
			}
			if !found {
				// nolint[lll]
				errs = append(errs, fmt.Sprintf("unknown registry '%v' in 'renames' (not defined in 'registries')", registryName))
			}

			if registryName == srcRegistryName {
				srcOriginal = registryImagePath
			}
		}

		if len(m.Registries) > 0 && len(srcOriginal) == 0 {
			errs = append(errs, fmt.Sprintf("could not find source registry in '%v'",
				rename))
		}

		registryNameSrc, imageNameSrc, _ := SplitRegistryImagePath(srcOriginal, knownRegistries)
		for _, registryImagePath := range rename {
			registryName, imageName, _ := SplitRegistryImagePath(registryImagePath, knownRegistries)
			if registryName != registryNameSrc {
				if imageName == imageNameSrc {
					errs = append(errs, fmt.Sprintf("redundant rename for %s",
						rename))
				}
			}
		}

	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf(strings.Join(errs, "\n"))
}

// PrettyValue creates a prettified string representation of MasterInventory.
func (mi *MasterInventory) PrettyValue() string {
	var b strings.Builder
	for regName, v := range *mi {
		fmt.Fprintln(&b, regName)
		imageNamesSorted := make([]string, 0)
		for imageName := range v {
			imageNamesSorted = append(imageNamesSorted, string(imageName))
		}
		sort.Strings(imageNamesSorted)
		for _, imageName := range imageNamesSorted {
			fmt.Fprintf(&b, "  %v\n", imageName)
			digestTags, ok := v[ImageName(imageName)]
			if !ok {
				continue
			}
			digestSorted := make([]string, 0)
			for digest := range digestTags {
				digestSorted = append(digestSorted, string(digest))
			}
			sort.Strings(digestSorted)
			for _, digest := range digestSorted {
				fmt.Fprintf(&b, "    %v\n", digest)
				tags, ok := digestTags[Digest(digest)]
				if !ok {
					continue
				}
				for _, tag := range tags {
					fmt.Fprintf(&b, "      %v\n", tag)
				}
			}
		}
	}
	return b.String()
}

// PrettyValue converts a Registry to a prettified string representation.
func (r *Registry) PrettyValue() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v (%v)\n", r.RegistryNameLong, r.RegistryName)
	fmt.Fprintln(&b, r.RegInvImageDigest.PrettyValue())
	return b.String()
}

// PrettyValue converts a RegInvImageDigest to a prettified string
// representation.
func (riid *RegInvImageDigest) PrettyValue() string {
	var b strings.Builder
	ids := make([]ImageDigest, 0)
	for id := range *riid {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		iID := string(ids[i].ImageName) + string(ids[i].Digest)
		jID := string(ids[j].ImageName) + string(ids[i].Digest)
		return iID < jID
	})
	for _, id := range ids {
		fmt.Fprintf(&b, "  %v\n", id.ImageName)
		fmt.Fprintf(&b, "    %v\n", id.Digest)
		tagArray, ok := (*riid)[id]
		if !ok {
			continue
		}
		tagArraySorted := make([]string, 0)
		for _, tag := range tagArray {
			tagArraySorted = append(tagArraySorted, string(tag))
		}
		sort.Strings(tagArraySorted)
		for _, tag := range tagArraySorted {
			fmt.Fprintf(&b, "      %v\n", tag)
		}
	}
	return b.String()
}

func getRegistryTagsWrapper(req stream.ExternalRequest) (*google.Tags, error) {

	var googleTags *google.Tags

	var getRegistryTagsCondition wait.ConditionFunc = func() (bool, error) {
		var err error

		googleTags, err = getRegistryTagsFrom(req)

		// We never return an error (err) in the second part of our return
		// argument, because we don't want to prematurely stop the
		// ExponentialBackoff() loop; we want it to continue looping until
		// either we get a well-formed tags value, or until it hits
		// ErrWaitTimeout. This is how ExponentialBackoff() uses the
		// ConditionFunc type.
		if err == nil && googleTags != nil && len(googleTags.Name) > 0 {
			return true, nil
		}
		return false, nil
	}

	err := wait.ExponentialBackoff(
		stream.BackoffDefault,
		getRegistryTagsCondition)

	if err != nil {
		klog.Error(err)
		return nil, err
	}

	return googleTags, nil
}

func getRegistryTagsFrom(req stream.ExternalRequest) (*google.Tags, error) {
	reader, _, err := req.StreamProducer.Produce()
	if err != nil {
		klog.Warning("error reading from stream:", err)
		// Skip google.Tags JSON parsing if there were errors reading from the
		// HTTP stream.
		return nil, err
	}

	// nolint[errcheck]
	defer req.StreamProducer.Close()

	tags, err := extractRegistryTags(reader)
	if err != nil {
		klog.Warning("error parsing *google.Tags from io.Reader handle:", err)
		return nil, err
	}

	return tags, nil
}

func getJSONSFromProcess(req stream.ExternalRequest) (cipJson.Objects, Errors) {
	var jsons cipJson.Objects
	errors := make(Errors, 0)
	stdoutReader, stderrReader, err := req.StreamProducer.Produce()
	if err != nil {
		errors = append(errors, Error{
			Context: "running process",
			Error:   err})
	}
	jsons, err = cipJson.Consume(stdoutReader)
	if err != nil {
		errors = append(errors, Error{
			Context: "parsing JSON",
			Error:   err})
	}
	be, err := ioutil.ReadAll(stderrReader)
	if err != nil {
		errors = append(errors, Error{
			Context: "reading process stderr",
			Error:   err})
	}
	if len(be) > 0 {
		errors = append(errors, Error{
			Context: "process had stderr",
			Error:   fmt.Errorf("%v", string(be))})
	}
	err = req.StreamProducer.Close()
	if err != nil {
		errors = append(errors, Error{
			Context: "closing process",
			Error:   err})
	}
	return jsons, errors
}

// IgnoreFromPromotion works by building up a new Inv type of those images that
// should NOT be bothered to be Promoted; these get ignored in the Promote()
// step later down the pipeline.
func (sc *SyncContext) IgnoreFromPromotion(regName RegistryName) {
	// regName will look like gcr.io/foo/bar/baz. We then look for the key
	// "foo/bar/baz".
	_, imgName, err := ParseContainerParts(string(regName))
	if err != nil {
		klog.Errorf("unable to ignore from promotion: %s\n", err)
		return
	}

	klog.Infof("ignoring from promotion: %s\n", imgName)
	sc.InvIgnore = append(sc.InvIgnore, ImageName(imgName))
}

// ParseContainerParts splits up a registry name into its component pieces.
// Unfortunately it has some specialized logic around particular inputs; this
// could be removed in a future promoter manifest version which could force the
// user to provide these delineations for us.
//
// nolint[gocyclo]
func ParseContainerParts(s string) (string, string, error) {
	parts := strings.Split(s, "/")
	if len(parts) <= 1 {
		goto error
	}
	// String may not have a double slash, or a trailing slash (which would
	// result in an empty substring).
	for _, part := range parts {
		if len(part) == 0 {
			goto error
		}
	}
	switch parts[0] {
	case "gcr.io":
		fallthrough
	case "asia.gcr.io":
		fallthrough
	case "eu.gcr.io":
		fallthrough
	case "us.gcr.io":
		if len(parts) == 2 {
			goto error
		}
		return strings.Join(parts[0:2], "/"), strings.Join(parts[2:], "/"), nil
	case "k8s.gcr.io":
		fallthrough
	case "staging-k8s.gcr.io":
		fallthrough
	default:
		return parts[0], strings.Join(parts[1:], "/"), nil
	}
error:
	return "", "", fmt.Errorf("invalid string '%s'", s)
}

// PopulateTokens populates the SyncContext's Tokens map with actual usable
// access tokens.
func (sc *SyncContext) PopulateTokens() error {
	for _, rc := range sc.RegistryContexts {
		token, err := gcloud.GetServiceAccountToken(
			rc.ServiceAccount,
			sc.UseServiceAccount)
		if err != nil {
			return err
		}
		tokenName := RootRepo(string(rc.Name))
		sc.Tokens[tokenName] = token
	}
	return nil
}

// GetTokenKeyDomainRepoPath splits a string by '/'. It's OK to do this because
// the RegistryName is already parsed against a Regex. (Maybe we should store
// the repo path separately when we do the initial parse...)
func GetTokenKeyDomainRepoPath(
	registryName RegistryName) (string, string, string) {

	s := string(registryName)
	i := strings.IndexByte(s, '/')
	key := ""
	if strings.Count(s, "/") < 2 {
		key = s
	} else {
		key = strings.Join(strings.Split(s, "/")[0:2], "/")
	}
	// key, domain, repository path
	return key, s[:i], s[i+1:]
}

// ReadAllRegistries reads all images in all registries in the SyncContext Each
// registry is composed of a image repositories, which can be recursive.
//
// To summarize: a docker *registry* is a set of *repositories*. It just so
// happens that to end-users, repositores resemble a tree structure because they
// are delineated by familiar filesystem-like "directory" paths.
//
// We use the term "registry" to mean the "root repository" in this program, but
// to be technically correct, for gcr.io/google-containers/foo/bar/baz:
//
//  - gcr.io is the registry
//  - gcr.io/google-containers is the toplevel repository (or "root" repo)
//  - gcr.io/google-containers/foo is a child repository
//  - gcr.io/google-containers/foo/bar is a child repository
//  - gcr.io/google-containers/foo/bar/baz is a child repository
//
// It may or may not be the case that the child repository is empty. E.g., if
// only one image gcr.io/google-containers/foo/bar/baz:1.0 exists in the entire
// registry, the foo/ and bar/ subdirs are empty repositories.
//
// The root repo, or "registry" in the loose sense, is what we care about. This
// is because in GCR, each root repo is given its own service account and
// credentials that extend to all child repos. And also in GCR, the name of the
// root repo is the same as the name of the GCP project that hosts it.
//
// NOTE: Repository names may overlap with image names. E.g., it may be in the
// example above that there are images named gcr.io/google-containers/foo:2.0
// and gcr.io/google-containers/foo/baz:2.0.
//
// nolint[gocyclo]
func (sc *SyncContext) ReadAllRegistries(
	mkProducer func(*SyncContext, RegistryContext) stream.Producer) {
	// Collect all images in sc.Inv (the src and dest registry names found in
	// the manifest).
	var populateRequests PopulateRequests = func(
		sc *SyncContext,
		reqs chan<- stream.ExternalRequest,
		wg *sync.WaitGroup) {

		// For each registry, start the very first root "repo" read call.
		for _, rc := range sc.RegistryContexts {
			// Create the request.
			var req stream.ExternalRequest
			req.RequestParams = rc
			req.StreamProducer = mkProducer(sc, rc)
			// Load request into the channel.
			wg.Add(1)
			reqs <- req
		}
	}
	var processRequest ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}

			// Now run the request (make network HTTP call with
			// ExponentialBackoff()).
			tagsStruct, err := getRegistryTagsWrapper(req)
			if err != nil {
				// Skip this request if it has unrecoverable errors (even after
				// ExponentialBackoff).
				reqRes.Errors = Errors{
					Error{
						Context: "getRegistryTagsWrapper",
						Error:   err}}
				requestResults <- reqRes
				wg.Add(-1)

				// Invalidate promotion conservatively for the subset of images
				// that touch this network request. If we have trouble reading
				// "foo" from a destination registry, do not bother trying to
				// promote it for all registries
				mutex.Lock()
				sc.IgnoreFromPromotion(req.RequestParams.(RegistryContext).Name)
				mutex.Unlock()

				continue
			}
			// Process the current repo.
			rName := req.RequestParams.(RegistryContext).Name
			digestTags := make(DigestTags)

			for digest, mfestInfo := range tagsStruct.Manifests {
				tagSlice := TagSlice{}
				for _, tag := range mfestInfo.Tags {
					tagSlice = append(tagSlice, Tag(tag))
				}
				digestTags[Digest(digest)] = tagSlice

				// Store MediaType.
				mutex.Lock()
				mediaType, err := supportedMediaType(mfestInfo.MediaType)
				if err != nil {
					fmt.Printf("digest %s: %s\n", digest, err)
				}
				sc.DigestMediaType[Digest(digest)] = mediaType
				mutex.Unlock()
			}

			// Only write an entry into our inventory if the entry has some
			// non-nil value for digestTags. This is because we only want to
			// populate the inventory with image names that have digests in
			// them, and exclude any image paths that are actually just folder
			// names without any images in them.
			if len(digestTags) > 0 {
				tokenKey, _, repoPath := GetTokenKeyDomainRepoPath(rName)

				// If there is no slash in the repoPath, it is a toplevel
				// imageName, and we can use tagsStruct.Name as-is. E.g.,
				// "gcr.io/foo/bar" will have ""
				var imageName ImageName
				if strings.Count(repoPath, "/") == 0 {
					imageName = ImageName(tagsStruct.Name)
				} else {
					upto := strings.IndexByte(repoPath, '/')
					imageName = ImageName(
						strings.TrimPrefix(
							tagsStruct.Name,
							(tagsStruct.Name[:upto])+"/"))
				}

				currentRepo := make(RegInvImage)
				currentRepo[imageName] = digestTags

				mutex.Lock()
				existingRegEntry := sc.Inv[RegistryName(tokenKey)]
				if len(existingRegEntry) == 0 {
					sc.Inv[RegistryName(tokenKey)] = currentRepo
				} else {
					sc.Inv[RegistryName(tokenKey)][imageName] = digestTags
				}
				mutex.Unlock()
			}

			reqRes.Errors = Errors{}
			requestResults <- reqRes

			// Process child repos.
			for _, childRepoName := range tagsStruct.Children {
				parentRC, _ := req.RequestParams.(RegistryContext)

				childRc := RegistryContext{
					Name: RegistryName(
						string(parentRC.Name) + "/" + childRepoName),
					// Inherit the service account used at the parent
					// (cascades down from toplevel to all subrepos). In the
					// future if the token exchange fails, we can refresh
					// the token here instead of using the one we inherit
					// below.
					ServiceAccount: parentRC.ServiceAccount,
					// Inherit the token as well.
					Token: parentRC.Token,
					// Don't need src, because we are just reading data
					// (don't care if it's the source reg or not).
				}

				var childReq stream.ExternalRequest
				childReq.RequestParams = childRc
				childReq.StreamProducer = mkProducer(sc, childRc)

				// Every time we "descend" into child nodes, increment the
				// semaphore.
				wg.Add(1)
				reqs <- childReq
			}
			// When we're done processing this node (req), decrement the
			// semaphore.
			wg.Add(-1)
		}
	}
	sc.ExecRequests(populateRequests, processRequest)
}

// MkReadRepositoryCmdReal creates a stream.Producer which makes a real call
// over the network.
func MkReadRepositoryCmdReal(
	sc *SyncContext,
	rc RegistryContext) stream.Producer {

	var sh stream.HTTP

	tokenKey, domain, repoPath := GetTokenKeyDomainRepoPath(rc.Name)

	httpReq, err := http.NewRequest(
		"GET",
		fmt.Sprintf("https://%s/v2/%s/tags/list", domain, repoPath),
		nil)

	if err != nil {
		klog.Fatalf(
			"could not create HTTP request for '%s/%s'",
			domain,
			repoPath)
	}
	rc.Token = sc.Tokens[RootRepo(tokenKey)]
	httpReq.SetBasicAuth("oauth2accesstoken", string(rc.Token))
	sh.Req = httpReq

	return &sh
}

// ExecRequests uses the Worker Pool pattern, where MaxConcurrentRequests
// determines the number of workers to spawn.
func (sc *SyncContext) ExecRequests(
	populateRequests PopulateRequests,
	processRequest ProcessRequest) {
	// Run requests.
	MaxConcurrentRequests := 10
	if sc.Threads > 0 {
		MaxConcurrentRequests = sc.Threads
	}
	mutex := &sync.Mutex{}
	reqs := make(chan stream.ExternalRequest, MaxConcurrentRequests)
	requestResults := make(chan RequestResult)
	// We have to use a WaitGroup, because even though we know beforehand the
	// number of workers, we don't know the number of jobs.
	wg := new(sync.WaitGroup)
	// Log any errors encountered.
	go func() {
		for reqRes := range requestResults {
			if len(reqRes.Errors) > 0 {
				klog.Errorf(
					"Request %v: error(s) encountered: %v\n",
					reqRes.Context,
					reqRes.Errors)
			} else {
				klog.Infof("Request %v: OK\n", reqRes.Context.RequestParams)
			}
		}
	}()
	for w := 0; w < MaxConcurrentRequests; w++ {
		go processRequest(sc, reqs, requestResults, wg, mutex)
	}
	// This can't be a goroutine, because the semaphore could be 0 by the time
	// wg.Wait() is called. So we need to block against the initial "seeding" of
	// workloads into the reqs channel.
	populateRequests(sc, reqs, wg)

	// Wait for all workers to finish draining the jobs.
	wg.Wait()
	close(reqs)

	// Close requestResults channel because no more new jobs are being created
	// (it's OK to close a channel even if it has nonzero length). On the other
	// hand, we cannot close the channel before we Wait() for the workers to
	// finish, because we would end up closing it too soon, and a worker would
	// end up trying to send a result to the already-closed channel.
	//
	// NOTE: This requestResults channel is only useful if we want a central
	// place to process how each request happened (good for maybe debugging slow
	// reqs? benchmarking?). If we just want to print something, for example,
	// whenever there's an error, we could do away with this channel and just
	// spit out to STDOUT wheneven we encounter an error, from whichever
	// goroutine (no need to put the error into a channel for consumption from a
	// single point).
	close(requestResults)
}

func extractRegistryTags(reader io.Reader) (*google.Tags, error) {

	tags := google.Tags{}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	for {
		err := decoder.Decode(&tags)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Println("DECODING ERROR:", err)
			return nil, err
		}
	}
	return &tags, nil
}

// Overwrite insert's b's values into a.
func (a DigestTags) Overwrite(b DigestTags) {
	for k, v := range b {
		a[k] = v
	}
}

// ToFQIN combines a RegistryName, ImageName, and Digest to form a
// fully-qualified image name (FQIN).
func ToFQIN(registryName RegistryName,
	imageName ImageName,
	digest Digest) string {

	return string(registryName) + "/" + string(imageName) + "@" + string(digest)
}

// ToPQIN converts a RegistryName, ImageName, and Tag to form a
// partially-qualified image name (PQIN). It's less exact than a FQIN because
// the digest information is not used.
func ToPQIN(registryName RegistryName, imageName ImageName, tag Tag) string {
	return string(registryName) + "/" + string(imageName) + ":" + string(tag)
}

// ToYAML displays a RegInvImage as YAML, but with the map items sorted
// alphabetically.
func (rii *RegInvImage) ToYAML() string {
	// Temporary structs that have slices, not maps.
	type digest struct {
		hash string
		tags []string
	}

	type image struct {
		name    string
		digests []digest
	}

	images := make([]image, 0)

	for name, dmap := range *rii {
		var digests []digest
		for k, v := range dmap {
			var tags []string
			for _, tag := range v {
				tags = append(tags, string(tag))
			}

			sort.Strings(tags)

			digests = append(digests, digest{
				hash: string(k),
				tags: tags,
			})
		}
		sort.Slice(digests, func(i, j int) bool {
			return digests[i].hash < digests[j].hash
		})

		images = append(images, image{
			name:    string(name),
			digests: digests,
		})
	}

	sort.Slice(images, func(i, j int) bool {
		return images[i].name < images[j].name
	})

	var b strings.Builder
	for _, image := range images {
		fmt.Fprintf(&b, "- name: %s\n", image.name)
		fmt.Fprintf(&b, "  dmap:\n")
		for _, digestEntry := range image.digests {
			fmt.Fprintf(&b, "    %s:", digestEntry.hash)
			if len(digestEntry.tags) > 0 {
				fmt.Fprintf(&b, "\n")
				for _, tag := range digestEntry.tags {
					fmt.Fprintf(&b, "    - %s\n", tag)
				}
			} else {
				fmt.Fprintf(&b, " []\n")
			}
		}
	}

	return b.String()
}

// ToLQIN converts a RegistryName and ImangeName to form a loosely-qualified
// image name (LQIN). Notice that it is missing tag information --- hence
// "loosely-qualified".
func ToLQIN(registryName RegistryName, imageName ImageName) string {
	return string(registryName) + "/" + string(imageName)
}

// GetLostImages gets all images in Manifest which are missing from src, and
// also logs them in the process.
func (sc *SyncContext) GetLostImages(mfest Manifest) RegInvImageDigest {
	src := sc.Inv[sc.SrcRegistry.Name].ToRegInvImageDigest()

	// If we had trouble reading from any of the images in the src registry,
	// remove them from the manifest to ignore them, so that we don't create any
	// false positives about lost images.
	klog.Warningf("removing images from the manifest that could not be read from the src registry: %s\n", sc.InvIgnore)
	mfestImagesFinal := []Image{}
	for _, image := range mfest.Images {
		for _, ignoreMe := range sc.InvIgnore {
			if image.ImageName != ignoreMe {
				mfestImagesFinal = append(mfestImagesFinal, image)
			}
		}
	}
	mfest.Images = mfestImagesFinal

	// lost = all images that cannot be found from src.
	lost := mfest.ToRegInvImageDigest().Minus(src)

	if len(lost) > 0 {
		klog.Errorf(
			"ERROR: Lost images (all images in Manifest that cannot be found"+
				" from src registry %v):\n",
			sc.SrcRegistry.Name)
		for imageDigest := range lost {
			fqin := ToFQIN(
				sc.SrcRegistry.Name,
				imageDigest.ImageName,
				imageDigest.Digest)
			klog.Errorf(
				"  %v in manifest is NOT in src registry!\n",
				fqin)
		}
		return lost
	}
	return nil
}

// NOTE: the issue with renaming an image is that we still need to know the
// original (source) image path because we need to use it in the actual call to
// add-tag during promotion. This means the request population logic must be
// aware of both the original and renamed version of the image name.
//
// By the time the mkPopReq is called, we are dealing with renamed inventory
// (riit). We need to detect this case from within this function and create the
// correct request populator --- namely, the source registry/Manifest's original
// image name must be used as the reference.
//
// The Manifest houses `registries` which contain the registry names. The
// RenamesDenormalized map will contain bidirectional entries for the rename.
// Since we already have the dest registry, we can just look up the destRegistry
// name + (renamed) image name in the RenamesDenormalized map; if we find an
// entry, then we just need to pick out the one for the source registry (it is
// guaranteed to be there). Now we have the source registry name in sc, so we
// can use that and the image name associated with the source registry to use as
// the source destination reg+image name.
func (sc *SyncContext) mkPopReq(
	destRC RegistryContext,
	riit RegInvImageTag,
	tp TagOp,
	oldDigest Digest,
	mkProducer PromotionContext,
	reqs chan<- stream.ExternalRequest,
	wg *sync.WaitGroup) {

	dest := sc.Inv[destRC.Name]
	for imageTag, digest := range riit {
		var req stream.ExternalRequest

		// Get original image name, if we detect a rename.
		srcImageName := imageTag.ImageName
		registryImagePath := RegistryImagePath(
			ToLQIN(destRC.Name, imageTag.ImageName))
		if renameMap, ok := sc.RenamesDenormalized[registryImagePath]; ok {
			if origImageName, ok := renameMap[sc.SrcRegistry.Name]; ok {
				srcImageName = origImageName
			} else {
				// This should never happen, as the src registry is guaranteed
				// (during manifest parsing and renames denormalization) to
				// exist as a key for every map in RenamesDenormalized.
				//
				// nolint[lll]
				klog.Warningf("could not find src registry in renameMap for image '%v'\n", imageTag.ImageName)
				continue
			}
		}

		req.StreamProducer = mkProducer(
			// We need to populate from the source to the dest.
			sc.SrcRegistry.Name,
			srcImageName,
			destRC,
			imageTag.ImageName,
			digest,
			imageTag.Tag,
			tp)
		tpStr := ""
		fqin := ToFQIN(
			destRC.Name,
			imageTag.ImageName,
			digest)
		movePointerOnly := false
		tpStr = tp.PrettyValue()
		if tp == Move {
			// It could be that we are either moving a tag by
			// uploading a new digest to dest, or that we are just
			// moving a tag from an already-uploaded digest in dest.
			// (The latter should be a lot quicker because there is no
			// need to copy the digest into the registry as it already
			// exists there). We should make a distinction and say so in
			// the logs.
			imageDigest := ImageDigest{
				ImageName: imageTag.ImageName,
				Digest:    digest,
			}
			_, movePointerOnly = dest.ToRegInvImageDigest()[imageDigest]
		}
		// Display msg when promoting.
		msg := fmt.Sprintf(
			"mkPopReq: %s tag: %s -> %s\n", tpStr, imageTag.Tag, fqin)
		// For moving tags, display a more verbose message (show the
		// what the tag currently points to).
		if tp == Move {
			msg = fmt.Sprintf(`%s tag: %s
  OLD -> %s/%s@%s
  NEW -> %s/%s@%s
`,
				tpStr,
				imageTag.Tag,
				destRC.Name,
				imageTag.ImageName,
				oldDigest,
				destRC.Name,
				imageTag.ImageName, digest)
			if movePointerOnly {
				msg += fmt.Sprintf(
					"    (Digest %v already exists in destination.)",
					digest)
			} else {
				msg += fmt.Sprintf(
					"    (Digest %v does not yet exist"+
						" in destination.)",
					digest)
			}
		}
		klog.Info(msg)

		// Save some information about this request. It's a bit like
		// HTTP "headers".
		req.RequestParams = PromotionRequest{
			tp,
			sc.SrcRegistry.Name,
			destRC.Name,
			destRC.ServiceAccount,
			srcImageName,
			imageTag.ImageName,
			digest,
			oldDigest,
			imageTag.Tag,
		}
		wg.Add(1)
		reqs <- req
	}

}

// SplitRegistryImagePath takes an arbitrary image path, and splits it into its
// component parts, according to the knownRegistries field. E.g., consider
// "gcr.io/foo/a/b/c" as the registryImagePath. If "gcr.io/foo" is in
// knownRegistries, then we split it into "gcr.io/foo" and "a/b/c". But if we
// were given "gcr.io/foo/a", we would split it into "gcr.io/foo/a" and "b/c".
func SplitRegistryImagePath(
	registryImagePath RegistryImagePath,
	knownRegistries []RegistryName) (RegistryName, ImageName, error) {

	for _, rName := range knownRegistries {
		if strings.HasPrefix(string(registryImagePath), string(rName)) {
			return rName, ImageName(registryImagePath[len(rName)+1:]), nil
		}
	}

	return RegistryName(""),
		ImageName(""),
		// nolint[lll]
		fmt.Errorf("could not determine registry name for '%v'", registryImagePath)
}

// DenormalizeRenames coverts the nested list of rename strings in the
// Manifest's `renames` field into a more query-friendly nested map (easier to
// perform lookups).
//
// It also checks the `renames` field in Manifest for errors.
//
// For examples of what this data will look like, see TestDenormalizeRenames.
//
// nolint[gocyclo]
func DenormalizeRenames(
	mfest Manifest,
	srcRegistryName RegistryName) (RenamesDenormalized, error) {

	knownRegistries := make([]RegistryName, 0)
	for _, r := range mfest.Registries {
		knownRegistries = append(knownRegistries, r.Name)
	}

	rd := make(RenamesDenormalized)
	for _, rename := range mfest.Renames {
		// Create "directed edges" that go in both directions --- from Src
		// to Dest, and Dest to Src. Because there can be multiple
		// destinations, we have to use a nested loop.
		for i, registryImagePathA := range rename {
			_, _, err := SplitRegistryImagePath(registryImagePathA, knownRegistries)
			if err != nil {
				return nil, err
			}

			// Create the directed edge targets.
			directedEdges := make(map[RegistryName]ImageName)
			for j, registryImagePathB := range rename {
				if j == i {
					continue
				}

				registryNameB, imageNameB, err := SplitRegistryImagePath(registryImagePathB, knownRegistries)
				if err != nil {
					return nil, err
				}
				directedEdges[registryNameB] = imageNameB
			}

			// Assign all targets to the starting point, registryImagePathA.
			rd[registryImagePathA] = directedEdges
		}
	}
	return rd, nil
}

func getRenameMap(
	lqin string,
	rd RenamesDenormalized) map[RegistryName]ImageName {

	m, ok := rd[RegistryImagePath(lqin)]
	if ok {
		return m
	}
	return nil
}

// applyRenames takes a given RegInvImageTag (riit) and converts it to a new
// RegInvImageTag (riitRenamed), by using information in RenamesDenormalized
// (rd).
func (riit *RegInvImageTag) applyRenames(
	srcReg RegistryName,
	destReg RegistryName,
	rd RenamesDenormalized) RegInvImageTag {

	riitRenamed := RegInvImageTag{}
	for imageTag, digest := range *riit {
		lqin := ToLQIN(srcReg, imageTag.ImageName)
		m := getRenameMap(lqin, rd)
		newName, ok := m[destReg]
		// Only rename if this dest registry requires renaming.
		if m != nil && ok {
			imageTagRenamed := imageTag
			imageTagRenamed.ImageName = newName
			riitRenamed[imageTagRenamed] = digest
		} else {
			riitRenamed[imageTag] = digest
		}
	}
	return riitRenamed
}

// mkPopulateRequestsForPromotion creates all requests necessary to reconcile
// the Manifest against the state of the world.
//
// nolint[gocyclo]
func mkPopulateRequestsForPromotion(
	mfest Manifest,
	promotionCandidatesIT RegInvImageTag,
	mkProducer PromotionContext) PopulateRequests {
	return func(sc *SyncContext, reqs chan<- stream.ExternalRequest, wg *sync.WaitGroup) {
		for _, registry := range mfest.Registries {
			if registry.Src {
				continue
			}

			// Promote images that are not in the destination registry.
			destIT := sc.Inv[registry.Name].ToRegInvImageTag()

			// For this dest registry, check if there are any renamings that
			// must occur. If so, modify the promotionCandidatesIT so that they
			// use the renamed versions. This is because destIT already has the
			// renamed versions (easier for set difference). We could go the
			// other way and rename the pertinent images in destIT to match the
			// naming scheme of the source registry (i.e., the raw `images`
			// field in the Manifest) --- one way is not obviously superior over
			// the other.
			promotionCandidatesITRenamed := promotionCandidatesIT.applyRenames(
				sc.SrcRegistry.Name, registry.Name, sc.RenamesDenormalized)

			promotionFiltered := promotionCandidatesITRenamed.Minus(destIT)
			promotionFilteredID := promotionFiltered.ToRegInvImageDigest()

			if len(promotionFilteredID) > 0 {
				klog.Infof(
					"To promote (after removing already-promoted images):\n%v",
					promotionFilteredID.PrettyValue())
				if sc.DryRun {
					klog.Infof(
						"---------- BEGIN PROMOTION (DRY RUN): %s: %s ----------\n",
						sc.ManifestPath,
						registry.Name)
				} else {
					klog.Infof("---------- BEGIN PROMOTION: %s: %s ----------\n",
						sc.ManifestPath,
						registry.Name)
				}
			} else {
				klog.Infof("Nothing to promote for %s.\n", registry.Name)
			}

			sc.mkPopReq(
				registry, // destRegistry
				promotionFiltered,
				Add,
				"",
				mkProducer,
				reqs,
				wg)

			// Audit the intersection (make sure already-existing tags in the
			// destination are pointing to the digest specified in the
			// manifest).
			toAudit := promotionCandidatesITRenamed.Intersection(destIT)
			for imageTag, digest := range toAudit {
				if liveDigest, ok := destIT[imageTag]; ok {
					// dest has this imageTag already; need to audit.
					pqin := ToPQIN(
						registry.Name,
						imageTag.ImageName,
						imageTag.Tag)
					if digest == liveDigest {
						// NOP if dest's imageTag is already pointing to the
						// same digest as in the manifest.

						klog.Infof(
							"skipping: image '%v' already points to the same"+
								" digest (%v) as in the Manifest\n",
							pqin,
							digest)
						continue
					} else {
						// Dest's tag is pointing to the wrong digest! Need to
						// move the tag.
						klog.Warningf(
							"Warning: image '%v' is pointing to the wrong"+
								" digest\n       got: %v\n  expected: %v\n",
							pqin,
							liveDigest,
							digest)

						sc.mkPopReq(
							registry,
							RegInvImageTag{imageTag: digest},
							Move,
							liveDigest,
							mkProducer,
							reqs,
							wg)
					}
				}
			}

			// Warn the user about extra tags:
			xtras := make([]string, 0)
			for imageTag := range destIT.Minus(promotionCandidatesITRenamed) {
				xtras = append(xtras, fmt.Sprintf(
					"%s/%s:%s",
					registry.Name,
					imageTag.ImageName,
					imageTag.Tag))
			}
			sort.Strings(xtras)
			for _, img := range xtras {
				klog.Warningf("Warning: extra tag found in dest: %s\n", img)
			}
		}
	}
}

// GetPromotionCandidatesIT returns those images that are due for promotion.
func (sc *SyncContext) GetPromotionCandidatesIT(
	mfest Manifest) RegInvImageTag {

	srcRegName := sc.SrcRegistry.Name
	src := sc.Inv[srcRegName]

	// If we had trouble reading from any of the images in the src/dest
	// registries, remove them from src to simulate them being missing; this has
	// the effect of removing from the act of promotion.
	for _, imgName := range sc.InvIgnore {
		delete(src, imgName)
	}

	// promotionCandidates = all images in the manifest that can be found from
	// src. But, this is filtered later to remove those ones that are already in
	// dest (see mkPopulateRequestsForPromotion).
	promotionCandidates := mfest.ToRegInvImageDigest().Intersection(src.ToRegInvImageDigest())

	klog.Infof(
		"To promote (intersection of Manifest and src registry %v):\n%v",
		sc.SrcRegistry.Name,
		promotionCandidates.PrettyValue())

	return promotionCandidates.ToRegInvImageTag()
}

// Promote performs container image promotion by realizing the intent in the
// Manifest.
func (sc *SyncContext) Promote(
	mfest Manifest,
	mkProducer func(
		RegistryName,
		ImageName,
		RegistryContext,
		ImageName,
		Digest,
		Tag,
		TagOp) stream.Producer,
	customProcessRequest *ProcessRequest) int {

	var exitCode int

	mfestID := (mfest.ToRegInvImageDigest())

	klog.Infof("Desired state:\n%v", mfestID.PrettyValue())
	lost := sc.GetLostImages(mfest)
	if len(lost) > 0 {
		// TODO: Have more meaningful exit codes (use iota?).
		exitCode = 1
	}

	promotionCandidatesIT := sc.GetPromotionCandidatesIT(mfest)

	var populateRequests = mkPopulateRequestsForPromotion(
		mfest,
		promotionCandidatesIT,
		mkProducer)

	var processRequest ProcessRequest
	var processRequestReal ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}
			errors := make(Errors, 0)
			stdoutReader, stderrReader, err := req.StreamProducer.Produce()
			if err != nil {
				errors = append(errors, Error{
					Context: "running process",
					Error:   err})
			}
			b, err := ioutil.ReadAll(stdoutReader)
			if err != nil {
				errors = append(errors, Error{
					Context: "reading process stdout",
					Error:   err})
			}
			be, err := ioutil.ReadAll(stderrReader)
			if err != nil {
				errors = append(errors, Error{
					Context: "reading process stderr",
					Error:   err})
			}
			// The add-tag has stderr; it uses stderr for debug messages, so
			// don't count it as an error. Instead just print it out as extra
			// info.
			klog.Infof("process stdout:\n%v\n", string(b))
			klog.Infof("process stderr:\n%v\n", string(be))
			err = req.StreamProducer.Close()
			if err != nil {
				errors = append(errors, Error{
					Context: "closing process",
					Error:   err})
			}
			reqRes.Errors = errors
			requestResults <- reqRes
			wg.Add(-1)
		}
	}

	captured := make(CapturedRequests)

	if sc.DryRun {
		processRequestDryRun := MkRequestCapturer(&captured)
		processRequest = processRequestDryRun
	} else {
		processRequest = processRequestReal
	}

	if customProcessRequest != nil {
		processRequest = *customProcessRequest
	}
	sc.ExecRequests(populateRequests, processRequest)

	if sc.DryRun {
		sc.PrintCapturedRequests(&captured)
	}

	return exitCode
}

// PrintCapturedRequests pretty-prints all given PromotionRequests.
func (sc *SyncContext) PrintCapturedRequests(capReqs *CapturedRequests) {
	prs := make([]PromotionRequest, 0)

	for req, count := range *capReqs {
		for i := 0; i < count; i++ {
			prs = append(prs, req)
		}
	}

	sort.Slice(prs, func(i, j int) bool {
		return prs[i].PrettyValue() < prs[j].PrettyValue()
	})
	if len(prs) > 0 {
		fmt.Println("")
		fmt.Println("captured reqs summary:")
		fmt.Println("")
		for _, pr := range prs {
			fmt.Printf("captured req: %v", pr.PrettyValue())
		}
		fmt.Println("")
	} else {
		fmt.Println("No requests captured.")
	}
}

// PrettyValue is a prettified string representation of a TagOp.
func (op *TagOp) PrettyValue() string {
	var tagOpPretty string
	switch *op {
	case Add:
		tagOpPretty = "ADD"
	case Move:
		tagOpPretty = "MOVE"
	case Delete:
		tagOpPretty = "DELETE"
	}
	return tagOpPretty
}

// PrettyValue is a prettified string representation of a PromotionRequest.
func (pr *PromotionRequest) PrettyValue() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v -> %v: Tag: '%v' <%v> %v",
		ToLQIN(pr.RegistrySrc, pr.ImageNameSrc),
		ToLQIN(pr.RegistryDest, pr.ImageNameDest),
		string(pr.Tag),
		pr.TagOp.PrettyValue(),
		string(pr.Digest))
	if len(pr.DigestOld) > 0 {
		fmt.Fprintf(&b, " (move from '%v')", string(pr.DigestOld))
	}
	fmt.Fprintf(&b, "\n")

	return b.String()
}

// MkRequestCapturer returns a function that simply records requests as they are
// captured (slured out from the reqs channel).
func MkRequestCapturer(captured *CapturedRequests) ProcessRequest {
	return func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		errs chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			pr := req.RequestParams.(PromotionRequest)
			mutex.Lock()
			if _, ok := (*captured)[pr]; ok {
				(*captured)[pr]++
			} else {
				(*captured)[pr] = 1
			}
			mutex.Unlock()
			wg.Add(-1)
		}
	}
}

// GarbageCollect deletes all images that are not referenced by Docker tags.
// nolint[gocyclo]
func (sc *SyncContext) GarbageCollect(
	mfest Manifest,
	mkProducer func(RegistryContext, ImageName, Digest) stream.Producer,
	customProcessRequest *ProcessRequest) {

	var populateRequests PopulateRequests = func(
		sc *SyncContext,
		reqs chan<- stream.ExternalRequest,
		wg *sync.WaitGroup) {

		for _, registry := range mfest.Registries {
			if registry.Name == sc.SrcRegistry.Name {
				continue
			}
			for imageName, digestTags := range sc.Inv[registry.Name] {
				for digest, tagArray := range digestTags {
					if len(tagArray) == 0 {
						var req stream.ExternalRequest
						req.StreamProducer = mkProducer(
							registry,
							imageName,
							digest)
						req.RequestParams = PromotionRequest{
							Delete,
							sc.SrcRegistry.Name,
							registry.Name,
							registry.ServiceAccount,

							// No source image name, because tag deletions
							// should only delete the what's in the
							// destination registry
							ImageName(""),

							imageName,
							digest,
							"",
							"",
						}
						wg.Add(1)
						reqs <- req
					}
				}
			}
		}
	}

	var processRequest ProcessRequest
	var processRequestReal ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}
			jsons, errors := getJSONSFromProcess(req)
			if len(errors) > 0 {
				reqRes.Errors = errors
				requestResults <- reqRes
				continue
			}
			for _, json := range jsons {
				klog.Info("DELETED image:", json)
			}
			reqRes.Errors = errors
			requestResults <- reqRes
			wg.Add(-1)
		}
	}

	captured := make(CapturedRequests)

	if sc.DryRun {
		processRequestDryRun := MkRequestCapturer(&captured)
		processRequest = processRequestDryRun
	} else {
		processRequest = processRequestReal
	}

	if customProcessRequest != nil {
		processRequest = *customProcessRequest
	}
	sc.ExecRequests(populateRequests, processRequest)

	if sc.DryRun {
		sc.PrintCapturedRequests(&captured)
	}
}

func supportedMediaType(v string) (cr.MediaType, error) {
	switch cr.MediaType(v) {
	case cr.DockerManifestList:
		return cr.DockerManifestList, nil
	case cr.DockerManifestSchema1:
		return cr.DockerManifestSchema1, nil
	case cr.DockerManifestSchema1Signed:
		return cr.DockerManifestSchema1Signed, nil
	case cr.DockerManifestSchema2:
		return cr.DockerManifestSchema2, nil
	default:
		return cr.MediaType(""), fmt.Errorf("unsupported MediaType %s", v)
	}
}

// ClearRepository wipes out all Docker images from a registry! Use with caution.
// nolint[gocyclo]
//
// TODO: Maybe split this into 2 parts, so that each part can be unit-tested
// separately (deletion of manifest lists vs deletion of other media types).
func (sc *SyncContext) ClearRepository(
	regName RegistryName,
	mfest Manifest,
	mkProducer func(RegistryContext, ImageName, Digest) stream.Producer,
	customProcessRequest *ProcessRequest) {

	// deleteRequestsPopulator returns a PopulateRequests that
	// varies by a predicate. Closure city!
	var deleteRequestsPopulator func(func(cr.MediaType) bool) PopulateRequests = func(predicate func(cr.MediaType) bool) PopulateRequests {

		var populateRequests PopulateRequests = func(
			sc *SyncContext,
			reqs chan<- stream.ExternalRequest,
			wg *sync.WaitGroup) {

			for _, registry := range mfest.Registries {
				// Skip over any registry that does not match the regName we want to
				// wipe.
				if registry.Name != regName {
					continue
				}
				for imageName, digestTags := range sc.Inv[registry.Name] {
					for digest, _ := range digestTags {
						mediaType, ok := sc.DigestMediaType[digest]
						if !ok {
							fmt.Println("could not detect MediaType of digest", digest)
							continue
						}
						if !predicate(mediaType) {
							fmt.Printf("skipping digest %s mediaType %s\n", digest, mediaType)
							continue
						}
						var req stream.ExternalRequest
						req.StreamProducer = mkProducer(
							registry,
							imageName,
							digest)
						req.RequestParams = PromotionRequest{
							Delete,
							"",
							registry.Name,
							registry.ServiceAccount,

							// No source image name, because tag deletions
							// should only delete the what's in the
							// destination registry
							ImageName(""),

							imageName,
							digest,
							"",
							"",
						}
						wg.Add(1)
						reqs <- req
					}
				}
			}
		}
		return populateRequests
	}

	var processRequest ProcessRequest
	var processRequestReal ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}
			jsons, errors := getJSONSFromProcess(req)
			if len(errors) > 0 {
				reqRes.Errors = errors
				requestResults <- reqRes
				// Don't skip over this request, because stderr here is ignorable.
				// continue
			}
			for _, json := range jsons {
				klog.Info("DELETED image:", json)
			}
			reqRes.Errors = errors
			requestResults <- reqRes
			wg.Add(-1)
		}
	}

	captured := make(CapturedRequests)

	if sc.DryRun {
		processRequestDryRun := MkRequestCapturer(&captured)
		processRequest = processRequestDryRun
	} else {
		processRequest = processRequestReal
	}

	if customProcessRequest != nil {
		processRequest = *customProcessRequest
	}

	var isEqualTo (func(cr.MediaType) func(cr.MediaType) bool) = func(want cr.MediaType) func(cr.MediaType) bool {
		return func(got cr.MediaType) bool {
			return want == got
		}
	}

	var isNotEqualTo (func(cr.MediaType) func(cr.MediaType) bool) = func(want cr.MediaType) func(cr.MediaType) bool {
		return func(got cr.MediaType) bool {
			return want != got
		}
	}

	// Avoid the GCR error that complains if you try to delete an image which is
	// referenced by a DockerManifestList, by first deleting all such manifest
	// lists.
	deleteManifestLists := deleteRequestsPopulator(isEqualTo(cr.DockerManifestList))
	sc.ExecRequests(deleteManifestLists, processRequest)
	deleteOthers := deleteRequestsPopulator(isNotEqualTo(cr.DockerManifestList))
	sc.ExecRequests(deleteOthers, processRequest)

	if sc.DryRun {
		sc.PrintCapturedRequests(&captured)
	}
}

// GetWriteCmd generates a gcloud command that is used to make modifications to
// a Docker Registry.
func GetWriteCmd(
	dest RegistryContext,
	useServiceAccount bool,
	srcRegistry RegistryName,
	srcImageName ImageName,
	destImageName ImageName,
	digest Digest,
	tag Tag,
	tp TagOp) []string {

	var cmd []string
	switch tp {
	case Move:
		// The "add-tag" command also moves tags as necessary.
		fallthrough
	case Add:
		cmd = []string{"gcloud",
			"--quiet",
			"--verbosity=debug",
			"container",
			"images",
			"add-tag",
			ToFQIN(srcRegistry, srcImageName, digest),
			ToPQIN(dest.Name, destImageName, tag)}
	case Delete:
		cmd = []string{"gcloud",
			"--quiet",
			"container",
			"images",
			"untag",
			ToPQIN(dest.Name, destImageName, tag)}
	}
	// Use the service account if it is desired.
	return gcloud.MaybeUseServiceAccount(
		dest.ServiceAccount,
		useServiceAccount,
		cmd)
}

// GetDeleteCmd generates the cloud command used to delete images (used for
// garbage collection).
func GetDeleteCmd(
	rc RegistryContext,
	useServiceAccount bool,
	img ImageName,
	digest Digest,
	force bool) []string {

	fqin := ToFQIN(rc.Name, img, digest)
	cmd := []string{
		"gcloud",
		"container",
		"images",
		"delete",
		fqin,
		"--format=json"}
	if force {
		cmd = append(cmd, "--force-delete-tags")
		cmd = append(cmd, "--quiet")
	}
	return gcloud.MaybeUseServiceAccount(
		rc.ServiceAccount,
		useServiceAccount,
		cmd)
}
