package pythonutils

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jfrog/build-info-go/entities"
	"github.com/jfrog/build-info-go/utils"
	gofrogcmd "github.com/jfrog/gofrog/io"
	"github.com/jfrog/gofrog/version"
)

const (
	Pip    PythonTool = "pip"
	Pipenv PythonTool = "pipenv"
	Poetry PythonTool = "poetry"
)

type PythonTool string

// Parse pythonDependencyPackage list to dependencies map. (mapping dependency to his child deps)
// Also returns a list of project's root dependencies
func parseDependenciesToGraph(packages []pythonDependencyPackage) (map[string][]string, []string, error) {
	// Create packages map.
	packagesMap := map[string][]string{}
	allSubPackages := map[string]bool{}
	for _, pkg := range packages {
		var subPackages []string
		for _, subPkg := range pkg.Dependencies {
			subPkgFullName := subPkg.Key + ":" + subPkg.InstalledVersion
			subPackages = append(subPackages, subPkgFullName)
			allSubPackages[subPkgFullName] = true
		}
		packagesMap[pkg.Package.Key+":"+pkg.Package.InstalledVersion] = subPackages
	}

	var topLevelPackagesList []string
	for pkgName := range packagesMap {
		if !allSubPackages[pkgName] {
			topLevelPackagesList = append(topLevelPackagesList, pkgName)
		}
	}
	return packagesMap, topLevelPackagesList, nil
}

// Structs for parsing the pip-dependency-map result.
type pythonDependencyPackage struct {
	Package      packageType   `json:"package,omitempty"`
	Dependencies []packageType `json:"dependencies,omitempty"`
}

type packageType struct {
	Key              string `json:"key,omitempty"`
	PackageName      string `json:"package_name,omitempty"`
	InstalledVersion string `json:"installed_version,omitempty"`
}

func GetPythonDependenciesFiles(tool PythonTool, args []string, log utils.Log, srcPath string) (map[string]entities.Dependency, error) {
	switch tool {
	case Pip, Pipenv:
		return InstallWithLogParsing(tool, args, log, srcPath)
	case Poetry:
		return extractPoetryDependenciesFiles(srcPath, args, log)
	default:
		return nil, errors.New(string(tool) + " commands are not supported.")
	}
}

func GetPythonDependencies(tool PythonTool, srcPath, localDependenciesPath string) (dependenciesGraph map[string][]string, topLevelDependencies []string, err error) {
	switch tool {
	case Pip:
		return getPipDependencies(srcPath, localDependenciesPath)
	case Pipenv:
		return getPipenvDependencies(srcPath)
	case Poetry:
		return getPoetryDependencies(srcPath)
	default:
		return nil, nil, errors.New(string(tool) + " commands are not supported.")
	}
}

func GetPackageName(tool PythonTool, srcPath string) (packageName string, err error) {
	switch tool {
	case Pip, Pipenv:
		return getPackageNameFromSetuppy(srcPath)
	case Poetry:
		packageName, _, err = getPackageNameFromPyproject(srcPath)
		return
	default:
		return "", errors.New(string(tool) + " commands are not supported.")
	}
}

// Before running this function, dependency IDs may be the file names of the resolved python packages.
// Update build info dependency IDs and the requestedBy field.
// allDependencies      - Dependency name to Dependency map
// dependenciesGraph    - Dependency graph as built by 'pipdeptree' or 'pipenv graph'
// topLevelPackagesList - The direct dependencies
// packageName          - The resolved package name of the Python project, may be empty if we couldn't resolve it
// moduleName           - The input module name from the user, or the packageName
func UpdateDepsIdsAndRequestedBy(dependenciesMap map[string]entities.Dependency, dependenciesGraph map[string][]string, topLevelPackagesList []string, packageName, moduleName string) {
	if packageName == "" {
		// Projects without setup.py
		dependenciesGraph[moduleName] = topLevelPackagesList
	} else if packageName != moduleName {
		// Projects with setup.py
		dependenciesGraph[moduleName] = dependenciesGraph[packageName]
	}
	rootModule := entities.Dependency{Id: moduleName, RequestedBy: [][]string{{}}}
	updateDepsIdsAndRequestedBy(rootModule, dependenciesMap, dependenciesGraph)
}

func updateDepsIdsAndRequestedBy(parentDependency entities.Dependency, dependenciesMap map[string]entities.Dependency, dependenciesGraph map[string][]string) {
	for _, childId := range dependenciesGraph[parentDependency.Id] {
		childName := childId[0:strings.Index(childId, ":")]
		if childDep, ok := dependenciesMap[childName]; ok {
			if childDep.NodeHasLoop() || len(childDep.RequestedBy) >= entities.RequestedByMaxLength {
				continue
			}
			// Update RequestedBy field from parent's RequestedBy.
			childDep.UpdateRequestedBy(parentDependency.Id, parentDependency.RequestedBy)

			// Set dependency type
			if childDep.Type == "" {
				fileType := ""
				if i := strings.LastIndex(childDep.Id, ".tar."); i != -1 {
					fileType = childDep.Id[i+1:]
				} else if i := strings.LastIndex(childDep.Id, "."); i != -1 {
					fileType = childDep.Id[i+1:]
				}
				childDep.Type = fileType
			}
			// Convert Id field from filename to dependency id
			childDep.Id = childId
			// Reassign map entry with new entry copy
			dependenciesMap[childName] = childDep
			// Run recursive call on child dependencies
			updateDepsIdsAndRequestedBy(childDep, dependenciesMap, dependenciesGraph)
		}
	}
}

func getFilePath(srcPath, fileName string) (string, error) {
	filePath := filepath.Join(srcPath, fileName)
	// Check if fileName exists.
	validPath, err := utils.IsFileExists(filePath, false)
	if err != nil || !validPath {
		return "", err
	}
	return filePath, nil
}

func InstallWithLogParsing(tool PythonTool, commandArgs []string, log utils.Log, srcPath string) (map[string]entities.Dependency, error) {
	if tool == Pipenv {
		// Add verbosity flag to pipenv commands to collect necessary data
		commandArgs = append(commandArgs, "-v")
	}
	installCmd := utils.NewCommand(string(tool), "install", commandArgs)
	installCmd.Dir = srcPath

	dependenciesMap := map[string]entities.Dependency{}

	// Create regular expressions for collecting information from log parsing.
	collectingRegexp := regexp.MustCompile(`^Collecting\s*(\w[\w-.]+)`)
	downloadingRegexp := regexp.MustCompile(`^\s*Downloading\s*([^\s]*)\s\(`)
	usingCachedRegexp := regexp.MustCompile(`\s*Using\scached\s([\S]+)\s\(`)
	alreadySatisfiedRegexp := regexp.MustCompile(`^Requirement\salready\ssatisfied:\s*(\w[\w-.]+)`)

	var packageName string
	expectingPackageFilePath := false

	// Extract downloaded package name.
	dependencyNameParser := gofrogcmd.CmdOutputPattern{
		RegExp: collectingRegexp,
		ExecFunc: func(pattern *gofrogcmd.CmdOutputPattern) (string, error) {
			// If this pattern matched a second time before downloaded-file-name was found, prompt a message.
			if expectingPackageFilePath {
				// This may occur when a package-installation file is saved in pip-cache-dir, thus not being downloaded during the installation.
				// Re-running pip-install with 'no-cache-dir' fixes this issue.
				log.Debug(fmt.Sprintf("Could not resolve download path for package: %s, continuing...", packageName))

				// Save package with empty file path.
				dependenciesMap[strings.ToLower(packageName)] = entities.Dependency{Id: ""}
			}

			// Check for out of bound results.
			if len(pattern.MatchedResults)-1 < 0 {
				log.Debug(fmt.Sprintf("Failed extracting package name from line: %s", pattern.Line))
				return pattern.Line, nil
			}

			// Save dependency information.
			expectingPackageFilePath = true
			packageName = pattern.MatchedResults[1]

			return pattern.Line, nil
		},
	}

	// Extract downloaded file, stored in Artifactory.
	downloadedFileParser := gofrogcmd.CmdOutputPattern{
		RegExp: downloadingRegexp,
		ExecFunc: func(pattern *gofrogcmd.CmdOutputPattern) (string, error) {
			// Check for out of bound results.
			if len(pattern.MatchedResults)-1 < 0 {
				log.Debug(fmt.Sprintf("Failed extracting download path from line: %s", pattern.Line))
				return pattern.Line, nil
			}

			// If this pattern matched before package-name was found, do not collect this path.
			if !expectingPackageFilePath {
				log.Debug(fmt.Sprintf("Could not resolve package name for download path: %s , continuing...", packageName))
				return pattern.Line, nil
			}

			// Save dependency information.
			filePath := pattern.MatchedResults[1]
			lastSlashIndex := strings.LastIndex(filePath, "/")
			var fileName string
			if lastSlashIndex == -1 {
				fileName = filePath
			} else {
				fileName = filePath[lastSlashIndex+1:]
			}
			dependenciesMap[strings.ToLower(packageName)] = entities.Dependency{Id: fileName}
			expectingPackageFilePath = false

			log.Debug(fmt.Sprintf("Found package: %s installed with: %s", packageName, fileName))
			return pattern.Line, nil
		},
	}

	cachedFileParser := gofrogcmd.CmdOutputPattern{
		RegExp:   usingCachedRegexp,
		ExecFunc: downloadedFileParser.ExecFunc,
	}

	// Extract already installed packages names.
	installedPackagesParser := gofrogcmd.CmdOutputPattern{
		RegExp: alreadySatisfiedRegexp,
		ExecFunc: func(pattern *gofrogcmd.CmdOutputPattern) (string, error) {
			// Check for out of bound results.
			if len(pattern.MatchedResults)-1 < 0 {
				log.Debug(fmt.Sprintf("Failed extracting package name from line: %s", pattern.Line))
				return pattern.Line, nil
			}

			// Save dependency with empty file name.
			dependenciesMap[strings.ToLower(pattern.MatchedResults[1])] = entities.Dependency{Id: ""}
			log.Debug(fmt.Sprintf("Found package: %s already installed", pattern.MatchedResults[1]))
			return pattern.Line, nil
		},
	}

	if tool == Pipenv {
		ver, err := getPipEnvVersion(log)
		if err != nil {
			return nil, err
		}
		if ver.Compare("2023.7.23") == 1 || ver.Compare("2023.7.23") == 0 {
			_, stderr, _, err := gofrogcmd.RunCmdWithOutputParser(installCmd, true)
			if err != nil {
				return nil, fmt.Errorf("failed running %s command with error: '%s - %s'", string(tool), err.Error(), stderr)
			}
			stderr = strings.ReplaceAll(stderr, "\n", "")
			usingCached := usingCachedRegexp.FindAllStringSubmatch(stderr, -1)
			for _, current := range usingCached {
				filePath := current[1]
				lastSlashIndex := strings.LastIndex(filePath, "/")
				var fileName string
				if lastSlashIndex == -1 {
					fileName = filePath
				} else {
					fileName = filePath[lastSlashIndex+1:]
				}
				dependenciesMap[strings.ToLower(packageName)] = entities.Dependency{Id: fileName}
			}
		}
	}

	// Execute command.
	_, errorOut, _, err := gofrogcmd.RunCmdWithOutputParser(installCmd, true, &dependencyNameParser, &downloadedFileParser, &cachedFileParser, &installedPackagesParser)
	if err != nil {
		return nil, fmt.Errorf("failed running %s command with error: '%s - %s'", string(tool), err.Error(), errorOut)
	}
	return dependenciesMap, nil
}

// todo: think if you need to pass the log.
func getPipEnvVersion(log utils.Log) (*version.Version, error) {
	if log == nil {
		log = &utils.NullLog{}
	}
	versionData, err := gofrogcmd.RunCmdOutput(utils.NewCommand(string(Pipenv), "--version", []string{}))
	if err != nil {
		return nil, err
	}
	_, versionData, found := strings.Cut(versionData, "version ")
	if !found {
		return nil, errors.New("couldn't find pipenv version")
	}
	versionData = strings.Replace(versionData, "\n", "", -1)
	return version.NewVersion(versionData), nil
}
