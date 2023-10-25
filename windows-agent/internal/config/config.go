// Package config manages configuration parameters. It manages the configuration for
// the Windows Agent so that only a single config file needs to exist.
package config

import (
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/config/registry"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/contracts"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/distros/database"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/distros/task"
	log "github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/grpc/logstreamer"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/tasks"
	"github.com/ubuntu/decorate"
	"golang.org/x/exp/slices"
)

const (
	registryPath = `Software\Canonical\UbuntuPro`

	fieldLandscapeClientConfig = "LandscapeClientConfig"
	fieldLandscapeAgentUID     = "LandscapeAgentUID"
)

// fieldsProToken contains the fields in the registry where each source will store its token.
var fieldsProToken = map[SubscriptionSource]string{
	SubscriptionOrganization:   "ProTokenOrg",
	SubscriptionUser:           "ProTokenUser",
	SubscriptionMicrosoftStore: "ProTokenStore",
}

// Registry abstracts away access to the windows registry.
type Registry interface {
	HKCUCreateKey(path string, access uint32) (newk uintptr, err error)
	HKCUOpenKey(path string, access uint32) (uintptr, error)
	CloseKey(k uintptr)
	ReadValue(k uintptr, field string) (value string, err error)
	WriteValue(k uintptr, field string, value string) (err error)
	WriteMultilineValue(k uintptr, field string, value string) (err error)
}

// Config manages configuration parameters. It is a wrapper around a dictionary
// that reads and updates the config file.
type Config struct {
	proTokens map[SubscriptionSource]string
	data      configData

	registry Registry

	mu *sync.Mutex
}

// configData is a bag of data unrelated to the subscription status.
type configData struct {
	landscapeClientConfig string
	landscapeAgentUID     string
}

// SubscriptionSource indicates the method the subscription was acquired.
type SubscriptionSource int

// Subscription types. Sorted in ascending order of precedence.
const (
	// SubscriptionNone -> no subscription.
	SubscriptionNone SubscriptionSource = iota

	// SubscriptionOrganization -> the subscription was obtained by introducing a pro token
	// via the registry by the sys admin.
	SubscriptionOrganization

	// SubscriptionUser -> the subscription was obtained by introducing a pro token
	// via the registry or the GUI.
	SubscriptionUser

	// SubscriptionMicrosoftStore -> the subscription was acquired via the Microsoft Store.
	SubscriptionMicrosoftStore

	// subscriptionMaxPriority is a sentinel value to make looping simpler.
	// It must always be the last value in the enum.
	subscriptionMaxPriority
)

type options struct {
	registry Registry
}

// Option is an optional argument for New.
type Option func(*options)

// WithRegistry allows for overriding the windows registry with a mock.
func WithRegistry(r Registry) Option {
	return func(o *options) {
		o.registry = r
	}
}

// New creates and initializes a new Config object.
func New(ctx context.Context, args ...Option) (m *Config) {
	var opts options

	for _, f := range args {
		f(&opts)
	}

	if opts.registry == nil {
		opts.registry = registry.Windows{}
	}

	m = &Config{
		registry:  opts.registry,
		mu:        &sync.Mutex{},
		proTokens: make(map[SubscriptionSource]string),
	}

	return m
}

// Subscription returns the ProToken and the method it was acquired with (if any).
func (c *Config) Subscription(ctx context.Context) (token string, source SubscriptionSource, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.load(ctx); err != nil {
		return "", SubscriptionNone, fmt.Errorf("could not load: %v", err)
	}

	token, source = c.subscription()
	return token, source, nil
}

// subscription is the unsafe version of Subscription. It returns the ProToken and the method it was acquired with (if any).
func (c *Config) subscription() (token string, source SubscriptionSource) {
	for src := subscriptionMaxPriority - 1; src > SubscriptionNone; src-- {
		token, ok := c.proTokens[src]
		if !ok {
			continue
		}
		if token == "" {
			continue
		}

		return token, src
	}

	return "", SubscriptionNone
}

// IsReadOnly returns whether the registry can be written to.
func (c *Config) IsReadOnly() (b bool, err error) {
	// CreateKey is equivalent to OpenKey if the key already existed
	k, err := c.registry.HKCUCreateKey(registryPath, registry.WRITE)
	if errors.Is(err, registry.ErrAccessDenied) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("could not open registry key: %w", err)
	}

	c.registry.CloseKey(k)
	return false, nil
}

// ProvisioningTasks returns a slice of all tasks to be submitted upon first contact with a distro.
func (c *Config) ProvisioningTasks(ctx context.Context, distroName string) ([]task.Task, error) {
	var taskList []task.Task

	// Refresh data from registry
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.load(ctx); err != nil {
		return nil, fmt.Errorf("could not load: %v", err)
	}

	// Ubuntu Pro attachment
	proToken, _ := c.subscription()
	taskList = append(taskList, tasks.ProAttachment{Token: proToken})

	if c.data.landscapeClientConfig == "" {
		// Landscape unregistration: always
		taskList = append(taskList, tasks.LandscapeConfigure{})
	} else if c.data.landscapeAgentUID != "" {
		// Landcape registration: only when we have a UID assigned
		taskList = append(taskList, tasks.LandscapeConfigure{
			Config:       c.data.landscapeClientConfig,
			HostagentUID: c.data.landscapeAgentUID,
		})
	}

	return taskList, nil
}

// SetSubscription overwrites the value of the pro token and the method with which it has been acquired.
func (c *Config) SetSubscription(ctx context.Context, proToken string, source SubscriptionSource) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Load before dumping to avoid overriding recent changes to registry
	if err := c.load(ctx); err != nil {
		return err
	}

	old := c.proTokens[source]
	c.proTokens[source] = proToken

	if err := c.dump(); err != nil {
		log.Errorf(ctx, "Could not update subscription in registry, token will be ignored: %v", err)
		c.proTokens[source] = old
		return err
	}

	return nil
}

// LandscapeClientConfig returns the value of the landscape server URL.
func (c *Config) LandscapeClientConfig(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.load(ctx); err != nil {
		return "", fmt.Errorf("could not load: %v", err)
	}

	return c.data.landscapeClientConfig, nil
}

// LandscapeAgentUID returns the UID assigned to this agent by the Landscape server.
// An empty string is returned if no UID has been assigned.
func (c *Config) LandscapeAgentUID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.load(ctx); err != nil {
		return "", fmt.Errorf("could not load: %v", err)
	}

	return c.data.landscapeAgentUID, nil
}

// SetLandscapeAgentUID overrides the Landscape agent UID.
func (c *Config) SetLandscapeAgentUID(ctx context.Context, uid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Load before dumping to avoid overriding recent changes to registry
	if err := c.load(ctx); err != nil {
		return err
	}

	old := c.data.landscapeAgentUID
	c.data.landscapeAgentUID = uid

	if err := c.dump(); err != nil {
		log.Errorf(ctx, "Could not update landscape agent UID in registry, UID will be ignored: %v", err)
		c.data.landscapeAgentUID = old
		return err
	}

	return nil
}

func (c *Config) load(ctx context.Context) (err error) {
	defer decorate.OnError(&err, "could not load data for Config")

	// Read registry
	proTokens, data, err := c.loadRegistry(ctx)
	if err != nil {
		return err
	}

	// Commit to loaded data
	c.proTokens = proTokens
	c.data = data

	return nil
}

func (c *Config) loadRegistry(ctx context.Context) (proTokens map[SubscriptionSource]string, data configData, err error) {
	defer decorate.OnError(&err, "could not load from registry")

	proTokens = make(map[SubscriptionSource]string)

	k, err := c.registry.HKCUOpenKey(registryPath, registry.READ)
	if errors.Is(err, registry.ErrKeyNotExist) {
		log.Debug(ctx, "Registry key does not exist, using default values")
		return proTokens, data, nil
	}
	if err != nil {
		return proTokens, data, err
	}
	defer c.registry.CloseKey(k)

	for source, field := range fieldsProToken {
		proToken, e := c.readValue(ctx, k, field)
		if e != nil {
			err = errors.Join(err, fmt.Errorf("could not read %q: %v", field, e))
			continue
		}

		if proToken == "" {
			continue
		}

		proTokens[source] = proToken
	}

	if err != nil {
		return nil, data, err
	}

	data.landscapeClientConfig, err = c.readValue(ctx, k, fieldLandscapeClientConfig)
	if err != nil {
		return proTokens, data, err
	}

	data.landscapeAgentUID, err = c.readValue(ctx, k, fieldLandscapeAgentUID)
	if err != nil {
		return proTokens, data, err
	}

	return proTokens, data, nil
}

func (c *Config) readValue(ctx context.Context, key uintptr, field string) (string, error) {
	value, err := c.registry.ReadValue(key, field)
	if errors.Is(err, registry.ErrFieldNotExist) {
		log.Debugf(ctx, "Registry value %q does not exist, defaulting to empty", field)
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

func (c *Config) dump() (err error) {
	defer decorate.OnError(&err, "could not store Config data")

	// CreateKey is equivalent to OpenKey if the key already existed
	k, err := c.registry.HKCUCreateKey(registryPath, registry.WRITE)
	if err != nil {
		return fmt.Errorf("could not open or create registry key: %w", err)
	}
	defer c.registry.CloseKey(k)

	for source, field := range fieldsProToken {
		err := c.registry.WriteValue(k, field, c.proTokens[source])
		if err != nil {
			return fmt.Errorf("could not write into registry key: %w", err)
		}
	}

	if err := c.registry.WriteMultilineValue(k, fieldLandscapeClientConfig, c.data.landscapeClientConfig); err != nil {
		return fmt.Errorf("could not write into registry key: %v", err)
	}

	if err := c.registry.WriteValue(k, fieldLandscapeAgentUID, c.data.landscapeAgentUID); err != nil {
		return fmt.Errorf("could not write into registry key: %v", err)
	}

	if err := c.registry.WriteValue(k, fieldLandscapeAgentUID, c.data.landscapeAgentUID); err != nil {
		return fmt.Errorf("could not write into registry key: %v", err)
	}

	return nil
}

// FetchMicrosoftStoreSubscription contacts Ubuntu Pro's contract server and the Microsoft Store
// to check if the user has an active subscription that provides a pro token. If so, that token is used.
func (c *Config) FetchMicrosoftStoreSubscription(ctx context.Context) (err error) {
	defer decorate.OnError(&err, "could not validate subscription against Microsoft Store")

	readOnly, err := c.IsReadOnly()
	if err != nil {
		return fmt.Errorf("could not detect if subscription is user-managed: %v", err)
	}

	if readOnly {
		// No need to contact the store because we cannot change the subscription
		return fmt.Errorf("subscription cannot be user-managed")
	}

	proToken, err := contracts.ProToken(ctx)
	if err != nil {
		return fmt.Errorf("could not get ProToken from Microsoft Store: %v", err)
	}

	if err := c.SetSubscription(ctx, proToken, SubscriptionMicrosoftStore); err != nil {
		return err
	}

	return nil
}

// UpdateRegistrySettings checks if any of the registry settings have changed since this function was last called.
// If so, new settings are pushed to the distros.
func (c *Config) UpdateRegistrySettings(ctx context.Context, cacheDir string, db *database.DistroDB) error {
	type getTask = func(*Config, context.Context, string, *database.DistroDB) (task.Task, error)

	// Collect tasks for updated settings
	var errs error
	var taskList []task.Task
	for _, f := range []getTask{(*Config).getTaskOnNewSubscription, (*Config).getTaskOnNewLandscape} {
		task, err := f(c, ctx, cacheDir, db)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if task != nil {
			taskList = append(taskList, task)
		}
	}

	if errs != nil {
		log.Warningf(ctx, "Could not obtain some updated registry settings: %v", errs)
	}

	// Apply tasks for updated settings
	errs = nil
	for _, d := range db.GetAll() {
		errs = errors.Join(errs, d.SubmitDeferredTasks(taskList...))
	}

	if errs != nil {
		return fmt.Errorf("could not submit new task to certain distros: %v", errs)
	}

	return nil
}

// getTaskOnNewSubscription checks if the subscription has changed since the last time it was called. If so, the new subscription
// is returned in the form of a task.
func (c *Config) getTaskOnNewSubscription(ctx context.Context, cacheDir string, db *database.DistroDB) (task.Task, error) {
	proToken, _, err := c.Subscription(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve current subscription: %v", err)
	}

	isNew, err := hasChanged(filepath.Join(cacheDir, "subscription.csum"), []byte(proToken))
	if err != nil {
		log.Warningf(ctx, "could not update checksum for Ubuntu Pro subscription: %v", err)
	}

	if !isNew {
		return nil, nil
	}

	log.Debug(ctx, "New Ubuntu Pro subscription settings detected in registry")
	return tasks.ProAttachment{Token: proToken}, nil
}

// getTaskOnNewLandscape checks if the Landscape settings has changed since the last time it was called. If so, the
// new Landscape settings are returned in the form of a task.
func (c *Config) getTaskOnNewLandscape(ctx context.Context, cacheDir string, db *database.DistroDB) (task.Task, error) {
	landscapeConf, err := c.LandscapeClientConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve current landscape config: %v", err)
	}

	landscapeUID, err := c.LandscapeAgentUID(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve current landscape UID: %v", err)
	}

	// We append them just so we can compute a combined checksum
	serialized := fmt.Sprintf("%s%s", landscapeUID, landscapeConf)

	isNew, err := hasChanged(filepath.Join(cacheDir, "landscape.csum"), []byte(serialized))
	if err != nil {
		log.Warningf(ctx, "could not update checksum for Landscape configuration: %v", err)
	}

	if !isNew {
		return nil, nil
	}

	log.Debug(ctx, "New Landscape settings detected in registry")

	// We must not register to landscape if we have no Landscape UID
	if landscapeConf != "" && landscapeUID == "" {
		log.Debug(ctx, "Ignoring new landscape settings: no Landscape agent UID")
		return nil, nil
	}

	return tasks.LandscapeConfigure{Config: landscapeConf, HostagentUID: landscapeUID}, nil
}

// hasChanged detects if the current value is different from the last time it was used.
// The return value is usable even if error is returned.
func hasChanged(cachePath string, newValue []byte) (new bool, err error) {
	var newCheckSum []byte
	if len(newValue) != 0 {
		tmp := sha512.Sum512(newValue)
		newCheckSum = tmp[:]
	}

	defer decorateUpdateCache(&new, &err, cachePath, newCheckSum)

	oldChecksum, err := os.ReadFile(cachePath)
	if errors.Is(err, fs.ErrNotExist) {
		// File not found: there was no value before
		oldChecksum = nil
	} else if err != nil {
		return true, fmt.Errorf("could not read old value: %v", err)
	}

	if slices.Equal(oldChecksum, newCheckSum) {
		return false, nil
	}

	return true, nil
}

// decorateUpdateCache acts depending on caller's return values (hence decorate).
// It stores the new checksum to the cachefile. Any errors are joined to *err.
func decorateUpdateCache(new *bool, err *error, cachePath string, newCheckSum []byte) {
	writeCacheErr := func() error {
		// If the value is empty, we remove the file.
		// This preserves this function's idempotency.
		if len(newCheckSum) == 0 {
			err := os.Remove(cachePath)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("could not remove old checksum: %v", err)
			}
			return nil
		}

		// Value is unchanged: don't write to file
		if !*new {
			return nil
		}

		// Update to file
		if err := os.WriteFile(cachePath, newCheckSum[:], 0600); err != nil {
			return fmt.Errorf("could not write checksum to cache: %v", err)
		}

		return nil
	}()

	*err = errors.Join(*err, writeCacheErr)
}
