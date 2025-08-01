package store

import (
	"fmt"
	"sort"

	_ "github.com/glebarez/sqlite" // provide the sqlite dialect to gorm via import
	"github.com/go-test/deep"
	"gorm.io/gorm"

	"github.com/anchore/grype/grype/db/internal/gormadapter"
	v5 "github.com/anchore/grype/grype/db/v5"
	"github.com/anchore/grype/grype/db/v5/store/model"
	"github.com/anchore/grype/internal/log"
	"github.com/anchore/grype/internal/stringutil"
)

// store holds an instance of the database connection
type store struct {
	db *gorm.DB
}

func models() []any {
	return []any{
		model.IDModel{},
		model.VulnerabilityModel{},
		model.VulnerabilityMetadataModel{},
		model.VulnerabilityMatchExclusionModel{},
	}
}

// New creates a new instance of the store.
func New(dbFilePath string, overwrite bool) (v5.Store, error) {
	db, err := gormadapter.Open(dbFilePath, gormadapter.WithTruncate(overwrite, models(), nil))
	if err != nil {
		return nil, err
	}

	return &store{
		db: db,
	}, nil
}

// GetID fetches the metadata about the databases schema version and build time.
func (s *store) GetID() (*v5.ID, error) {
	var models []model.IDModel
	result := s.db.Find(&models)
	if result.Error != nil {
		return nil, result.Error
	}

	switch {
	case len(models) > 1:
		return nil, fmt.Errorf("found multiple DB IDs")
	case len(models) == 1:
		id, err := models[0].Inflate()
		if err != nil {
			return nil, err
		}
		return &id, nil
	}

	return nil, nil
}

// SetID stores the databases schema version and build time.
func (s *store) SetID(id v5.ID) error {
	var ids []model.IDModel

	// replace the existing ID with the given one
	s.db.Find(&ids).Delete(&ids)

	m := model.NewIDModel(id)
	result := s.db.Create(&m)

	if result.RowsAffected != 1 {
		return fmt.Errorf("unable to add id (%d rows affected)", result.RowsAffected)
	}

	return result.Error
}

// GetVulnerabilityNamespaces retrieves all possible namespaces from the database.
func (s *store) GetVulnerabilityNamespaces() ([]string, error) {
	var names []string
	result := s.db.Model(&model.VulnerabilityMetadataModel{}).Distinct().Pluck("namespace", &names)
	return names, result.Error
}

// GetVulnerability retrieves vulnerabilities by namespace and id
func (s *store) GetVulnerability(namespace, id string) ([]v5.Vulnerability, error) {
	var models []model.VulnerabilityModel

	query := s.db.Where("id = ?", id)

	if namespace != "" {
		query = query.Where("namespace = ?", namespace)
	}

	result := query.Find(&models)

	vulnerabilities := make([]v5.Vulnerability, len(models))
	for idx, m := range models {
		vulnerability, err := m.Inflate()
		if err != nil {
			return nil, err
		}
		vulnerabilities[idx] = vulnerability
	}

	return vulnerabilities, result.Error
}

// SearchForVulnerabilities retrieves vulnerabilities by namespace and package
func (s *store) SearchForVulnerabilities(namespace, packageName string) ([]v5.Vulnerability, error) {
	var models []model.VulnerabilityModel

	result := s.db.Where("namespace = ? AND package_name = ?", namespace, packageName).Find(&models)

	vulnerabilities := make([]v5.Vulnerability, len(models))
	for idx, m := range models {
		vulnerability, err := m.Inflate()
		if err != nil {
			return nil, err
		}
		vulnerabilities[idx] = vulnerability
	}

	return vulnerabilities, result.Error
}

// AddVulnerability saves one or more vulnerabilities into the sqlite3 store.
func (s *store) AddVulnerability(vulnerabilities ...v5.Vulnerability) error {
	for _, vulnerability := range vulnerabilities {
		m := model.NewVulnerabilityModel(vulnerability)

		result := s.db.Create(&m)
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return fmt.Errorf("unable to add vulnerability (%d rows affected)", result.RowsAffected)
		}
	}
	return nil
}

// GetVulnerabilityMetadata retrieves metadata for the given vulnerability ID relative to a specific record source.
func (s *store) GetVulnerabilityMetadata(id, namespace string) (*v5.VulnerabilityMetadata, error) {
	var models []model.VulnerabilityMetadataModel

	result := s.db.Where(&model.VulnerabilityMetadataModel{ID: id, Namespace: namespace}).Find(&models)
	if result.Error != nil {
		return nil, result.Error
	}

	switch {
	case len(models) > 1:
		return nil, fmt.Errorf("found multiple metadatas for single ID=%q Namespace=%q", id, namespace)
	case len(models) == 1:
		metadata, err := models[0].Inflate()
		if err != nil {
			return nil, err
		}

		return &metadata, nil
	}

	return nil, nil
}

// AddVulnerabilityMetadata stores one or more vulnerability metadata models into the sqlite DB.
//
//nolint:gocognit
func (s *store) AddVulnerabilityMetadata(metadata ...v5.VulnerabilityMetadata) error {
	for _, m := range metadata {
		existing, err := s.GetVulnerabilityMetadata(m.ID, m.Namespace)
		if err != nil {
			return fmt.Errorf("failed to verify existing entry: %w", err)
		}

		if existing != nil {
			// merge with the existing entry

			switch {
			case existing.Severity != m.Severity:
				return fmt.Errorf("existing metadata has mismatched severity (%q!=%q)", existing.Severity, m.Severity)
			case existing.Description != m.Description:
				return fmt.Errorf("existing metadata has mismatched description (%q!=%q)", existing.Description, m.Description)
			}

		incoming:
			// go through all incoming CVSS and see if they are already stored.
			// If they exist already in the database then skip adding them,
			// preventing a duplicate
			for _, incomingCvss := range m.Cvss {
				for _, existingCvss := range existing.Cvss {
					if len(deep.Equal(incomingCvss, existingCvss)) == 0 {
						// duplicate found, so incoming CVSS shouldn't get added
						continue incoming
					}
				}
				// a duplicate CVSS entry wasn't found, so append the incoming CVSS
				existing.Cvss = append(existing.Cvss, incomingCvss)
			}

			links := stringutil.NewStringSetFromSlice(existing.URLs)
			for _, l := range m.URLs {
				links.Add(l)
			}

			existing.URLs = links.ToSlice()
			sort.Strings(existing.URLs)

			newModel := model.NewVulnerabilityMetadataModel(*existing)
			result := s.db.Save(&newModel)

			if result.RowsAffected != 1 {
				return fmt.Errorf("unable to merge vulnerability metadata (%d rows affected)", result.RowsAffected)
			}

			if result.Error != nil {
				return result.Error
			}
		} else {
			// this is a new entry
			newModel := model.NewVulnerabilityMetadataModel(m)
			result := s.db.Create(&newModel)
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return fmt.Errorf("unable to add vulnerability metadata (%d rows affected)", result.RowsAffected)
			}
		}
	}
	return nil
}

// GetVulnerabilityMatchExclusion retrieves one or more vulnerability match exclusion records given a vulnerability identifier.
func (s *store) GetVulnerabilityMatchExclusion(id string) ([]v5.VulnerabilityMatchExclusion, error) {
	var models []model.VulnerabilityMatchExclusionModel

	result := s.db.Where("id = ?", id).Find(&models)

	var exclusions []v5.VulnerabilityMatchExclusion
	for _, m := range models {
		exclusion, err := m.Inflate()
		if err != nil {
			return nil, err
		}

		if exclusion != nil {
			exclusions = append(exclusions, *exclusion)
		}
	}

	return exclusions, result.Error
}

// AddVulnerabilityMatchExclusion saves one or more vulnerability match exclusion records into the sqlite3 store.
func (s *store) AddVulnerabilityMatchExclusion(exclusions ...v5.VulnerabilityMatchExclusion) error {
	for _, exclusion := range exclusions {
		m := model.NewVulnerabilityMatchExclusionModel(exclusion)

		result := s.db.Create(&m)
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return fmt.Errorf("unable to add vulnerability match exclusion (%d rows affected)", result.RowsAffected)
		}
	}

	return nil
}

func (s *store) Close() error {
	log.Debug("optimizing database settings for memory-efficient VACUUM")

	// Reduce memory footprint for VACUUM operation
	memoryEfficientStatements := []string{
		"PRAGMA cache_size = -32768",     // 32MB instead of 1GB
		"PRAGMA temp_store = FILE",       // Use disk for temp storage
		"PRAGMA mmap_size = 67108864",    // 64MB instead of 1GB
		"PRAGMA journal_mode = TRUNCATE", // Disk-based journal, no directory modifications
	}

	for _, stmt := range memoryEfficientStatements {
		if err := s.db.Exec(stmt).Error; err != nil {
			log.WithFields("statement", stmt, "error", err).Warn("failed to apply memory optimization")
		} else {
			log.WithFields("statement", stmt).Debug("applied memory optimization")
		}
	}

	log.Debug("starting database VACUUM operation")
	s.db.Exec("VACUUM;")
	log.Debug("database VACUUM operation completed")

	sqlDB, _ := s.db.DB()
	if sqlDB != nil {
		_ = sqlDB.Close()
	}

	return nil
}

// GetAllVulnerabilities gets all vulnerabilities in the database
func (s *store) GetAllVulnerabilities() (*[]v5.Vulnerability, error) {
	var models []model.VulnerabilityModel
	if result := s.db.Find(&models); result.Error != nil {
		return nil, result.Error
	}
	vulns := make([]v5.Vulnerability, len(models))
	for idx, m := range models {
		vuln, err := m.Inflate()
		if err != nil {
			return nil, err
		}
		vulns[idx] = vuln
	}
	return &vulns, nil
}

// GetAllVulnerabilityMetadata gets all vulnerability metadata in the database
func (s *store) GetAllVulnerabilityMetadata() (*[]v5.VulnerabilityMetadata, error) {
	var models []model.VulnerabilityMetadataModel
	if result := s.db.Find(&models); result.Error != nil {
		return nil, result.Error
	}
	metadata := make([]v5.VulnerabilityMetadata, len(models))
	for idx, m := range models {
		data, err := m.Inflate()
		if err != nil {
			return nil, err
		}
		metadata[idx] = data
	}
	return &metadata, nil
}

// DiffStore creates a diff between the current sql database and the given store
func (s *store) DiffStore(targetStore v5.StoreReader) (*[]v5.Diff, error) {
	// 7 stages, one for each step of the diff process (stages)
	rowsProgress, diffItems, stager := trackDiff(7)

	stager.Current = "reading target vulnerabilities"
	targetVulns, err := targetStore.GetAllVulnerabilities()
	rowsProgress.Increment()
	if err != nil {
		return nil, err
	}

	stager.Current = "reading base vulnerabilities"
	baseVulns, err := s.GetAllVulnerabilities()
	rowsProgress.Increment()
	if err != nil {
		return nil, err
	}

	stager.Current = "preparing"
	baseVulnPkgMap := buildVulnerabilityPkgsMap(baseVulns)
	targetVulnPkgMap := buildVulnerabilityPkgsMap(targetVulns)

	stager.Current = "comparing vulnerabilities"
	allDiffsMap := diffVulnerabilities(baseVulns, targetVulns, baseVulnPkgMap, targetVulnPkgMap, diffItems)

	stager.Current = "reading base metadata"
	baseMetadata, err := s.GetAllVulnerabilityMetadata()
	if err != nil {
		return nil, err
	}
	rowsProgress.Increment()

	stager.Current = "reading target metadata"
	targetMetadata, err := targetStore.GetAllVulnerabilityMetadata()
	if err != nil {
		return nil, err
	}
	rowsProgress.Increment()

	stager.Current = "comparing metadata"
	metaDiffsMap := diffVulnerabilityMetadata(baseMetadata, targetMetadata, baseVulnPkgMap, targetVulnPkgMap, diffItems)
	for k, diff := range *metaDiffsMap {
		(*allDiffsMap)[k] = diff
	}
	allDiffs := []v5.Diff{}
	for _, diff := range *allDiffsMap {
		allDiffs = append(allDiffs, *diff)
	}

	rowsProgress.SetCompleted()
	diffItems.SetCompleted()

	return &allDiffs, nil
}
