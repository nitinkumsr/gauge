// Copyright 2015 ThoughtWorks, Inc.

// This file is part of Gauge.

// Gauge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Gauge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with Gauge.  If not, see <http://www.gnu.org/licenses/>.

package infoGatherer

import (
	"io/ioutil"
	"path/filepath"
	"sync"

	"os"

	"github.com/fsnotify/fsnotify"
	"github.com/getgauge/common"
	"github.com/getgauge/gauge/config"
	"github.com/getgauge/gauge/gauge"
	"github.com/getgauge/gauge/gauge_messages"
	"github.com/getgauge/gauge/logger"
	"github.com/getgauge/gauge/parser"
	"github.com/getgauge/gauge/util"
)

// SpecInfoGatherer contains the caches for specs, concepts, and steps
type SpecInfoGatherer struct {
	waitGroup         sync.WaitGroup
	conceptDictionary *gauge.ConceptDictionary
	specsCache        specsCache
	conceptsCache     conceptCache
	stepsCache        stepsCache
	SpecDirs          []string
}

type conceptCache struct {
	mutex    sync.RWMutex
	concepts map[string][]*gauge.Concept
}

type stepsCache struct {
	mutex      sync.RWMutex
	stepValues map[string][]*gauge.StepValue
}

type specsCache struct {
	mutex       sync.RWMutex
	specDetails map[string]*SpecDetail
}

type SpecDetail struct {
	Spec *gauge.Specification
	Errs []parser.ParseError
}

func (d *SpecDetail) HasSpec() bool {
	return d.Spec != nil && d.Spec.Heading != nil
}

// MakeListOfAvailableSteps initializes all the SpecInfoGatherer caches
func (s *SpecInfoGatherer) MakeListOfAvailableSteps() {
	go s.watchForFileChanges()
	s.waitGroup.Wait()

	// Concepts parsed first because we need to create a concept dictionary that spec parsing can use
	s.initConceptsCache()
	s.initSpecsCache()
	s.initStepsCache()
}

func (s *SpecInfoGatherer) initSpecsCache() {
	details := s.getParsedSpecs(getSpecFiles(s.SpecDirs))

	s.specsCache.mutex.Lock()
	defer s.specsCache.mutex.Unlock()

	s.specsCache.specDetails = make(map[string]*SpecDetail, 0)

	logger.APILog.Info("Initializing specs cache with %d specs", len(details))
	for _, d := range details {
		logger.APILog.Debug("Adding specs from %s", d.Spec.FileName)
		s.addToSpecsCache(d.Spec.FileName, d)
	}
}

func getSpecFiles(specs []string) []string {
	var specFiles []string
	for _, dir := range specs {
		specFiles = append(specFiles, util.FindSpecFilesIn(dir)...)
	}
	return specFiles
}

func (s *SpecInfoGatherer) initConceptsCache() {
	s.conceptsCache.mutex.Lock()
	defer s.conceptsCache.mutex.Unlock()

	parsedConcepts := s.getParsedConcepts()
	s.conceptsCache.concepts = make(map[string][]*gauge.Concept, 0)
	logger.APILog.Info("Initializing concepts cache with %d concepts", len(parsedConcepts))
	for _, concept := range parsedConcepts {
		logger.APILog.Debug("Adding concepts from %s", concept.FileName)
		s.addToConceptsCache(concept.FileName, concept)
	}
}

func (s *SpecInfoGatherer) initStepsCache() {
	s.stepsCache.mutex.Lock()
	defer s.stepsCache.mutex.Unlock()

	s.stepsCache.stepValues = make(map[string][]*gauge.StepValue, 0)
	stepsFromSpecsMap := s.getStepsFromCachedSpecs()
	stepsFromConceptsMap := s.getStepsFromCachedConcepts()

	for filename, steps := range stepsFromConceptsMap {
		s.addToStepsCache(filename, steps)
	}
	for filename, steps := range stepsFromSpecsMap {
		s.addToStepsCache(filename, steps)
	}
	logger.APILog.Info("Initializing steps cache with %d steps", len(stepsFromSpecsMap)+len(stepsFromConceptsMap))
}

func (s *SpecInfoGatherer) addToSpecsCache(key string, value *SpecDetail) {
	s.specsCache.specDetails[key] = value
}

func (s *SpecInfoGatherer) addToConceptsCache(key string, value *gauge.Concept) {
	if s.conceptsCache.concepts[key] == nil {
		s.conceptsCache.concepts[key] = make([]*gauge.Concept, 0)
	}
	s.conceptsCache.concepts[key] = append(s.conceptsCache.concepts[key], value)
}

func (s *SpecInfoGatherer) deleteFromConceptDictionary(file string) {
	for _, c := range s.conceptsCache.concepts[file] {
		delete(s.conceptDictionary.ConceptsMap, c.ConceptStep.Value)
	}
}
func (s *SpecInfoGatherer) addToStepsCache(fileName string, allSteps []*gauge.StepValue) {
	s.stepsCache.stepValues[fileName] = allSteps
}

func (s *SpecInfoGatherer) getParsedSpecs(specFiles []string) []*SpecDetail {
	if s.conceptDictionary == nil {
		s.conceptDictionary = gauge.NewConceptDictionary()
	}
	parsedSpecs, parseResults := parser.ParseSpecFiles(specFiles, s.conceptDictionary, gauge.NewBuildErrors())
	specs := make(map[string]*SpecDetail)

	for _, spec := range parsedSpecs {
		specs[spec.FileName] = &SpecDetail{Spec: spec}
	}
	for _, v := range parseResults {
		_, ok := specs[v.FileName]
		if !ok {
			specs[v.FileName] = &SpecDetail{Spec: &gauge.Specification{FileName: v.FileName}}
		}
		specs[v.FileName].Errs = append(v.CriticalErrors, v.ParseErrors...)
	}
	details := make([]*SpecDetail, 0)
	for _, d := range specs {
		details = append(details, d)
	}
	return details
}

func (s *SpecInfoGatherer) getParsedConcepts() map[string]*gauge.Concept {
	var result *parser.ParseResult
	s.conceptDictionary, result = parser.CreateConceptsDictionary()
	handleParseFailures([]*parser.ParseResult{result})
	return s.conceptDictionary.ConceptsMap
}

func (s *SpecInfoGatherer) getStepsFromCachedSpecs() map[string][]*gauge.StepValue {
	s.specsCache.mutex.RLock()
	defer s.specsCache.mutex.RUnlock()

	var stepsFromSpecsMap = make(map[string][]*gauge.StepValue, 0)
	for _, detail := range s.specsCache.specDetails {
		stepsFromSpecsMap[detail.Spec.FileName] = append(stepsFromSpecsMap[detail.Spec.FileName], getStepsFromSpec(detail.Spec)...)
	}
	return stepsFromSpecsMap
}

func (s *SpecInfoGatherer) getStepsFromCachedConcepts() map[string][]*gauge.StepValue {
	var stepsFromConceptMap = make(map[string][]*gauge.StepValue, 0)
	s.conceptsCache.mutex.RLock()
	defer s.conceptsCache.mutex.RUnlock()
	for _, conceptList := range s.conceptsCache.concepts {
		for _, concept := range conceptList {
			stepsFromConceptMap[concept.FileName] = append(stepsFromConceptMap[concept.FileName], getStepsFromConcept(concept)...)
		}
	}
	return stepsFromConceptMap
}

func (s *SpecInfoGatherer) OnSpecFileModify(file string) {
	logger.APILog.Info("Spec file added / modified: %s", file)

	details := s.getParsedSpecs([]string{file})
	s.specsCache.mutex.Lock()
	s.addToSpecsCache(file, details[0])
	s.specsCache.mutex.Unlock()

	var steps []*gauge.StepValue
	for _, step := range getStepsFromSpec(details[0].Spec) {
		con := s.conceptDictionary.Search(step.StepValue)
		if con == nil {
			steps = append(steps, step)
		}
	}
	s.stepsCache.mutex.Lock()
	s.addToStepsCache(file, steps)
	s.stepsCache.mutex.Unlock()
}

func (s *SpecInfoGatherer) OnConceptFileModify(file string) {
	s.conceptsCache.mutex.Lock()
	defer s.conceptsCache.mutex.Unlock()

	logger.APILog.Info("Concept file added / modified: %s", file)
	s.deleteFromConceptDictionary(file)
	concepts, parseErrors := parser.AddConcepts(file, s.conceptDictionary)
	if len(parseErrors) > 0 {
		for _, err := range parseErrors {
			logger.APILog.Error("Error parsing concepts: ", err)
		}
		return
	}
	s.conceptsCache.concepts[file] = make([]*gauge.Concept, 0)
	for _, concept := range concepts {
		c := gauge.Concept{ConceptStep: concept, FileName: file}
		s.addToConceptsCache(file, &c)
		stepsFromConcept := getStepsFromConcept(&c)
		s.addToStepsCache(file, stepsFromConcept)
	}
}

func (s *SpecInfoGatherer) onSpecFileRemove(file string) {
	logger.APILog.Info("Spec file removed: %s", file)
	s.specsCache.mutex.Lock()
	defer s.specsCache.mutex.Unlock()
	delete(s.specsCache.specDetails, file)
}

func (s *SpecInfoGatherer) onConceptFileRemove(file string) {
	logger.APILog.Info("Concept file removed: %s", file)
	s.conceptsCache.mutex.Lock()
	defer s.conceptsCache.mutex.Unlock()
	for _, c := range s.conceptsCache.concepts[file] {
		delete(s.conceptDictionary.ConceptsMap, c.ConceptStep.Value)
	}
	delete(s.conceptsCache.concepts, file)
}

func (s *SpecInfoGatherer) onFileAdd(watcher *fsnotify.Watcher, file string) {
	if util.IsDir(file) {
		addDirToFileWatcher(watcher, file)
	}
	s.onFileModify(watcher, file)
}

func (s *SpecInfoGatherer) onFileModify(watcher *fsnotify.Watcher, file string) {
	if util.IsSpec(file) {
		s.OnSpecFileModify(file)
	} else if util.IsConcept(file) {
		s.OnConceptFileModify(file)
	}
}

func (s *SpecInfoGatherer) onFileRemove(watcher *fsnotify.Watcher, file string) {
	if util.IsSpec(file) {
		s.onSpecFileRemove(file)
	} else if util.IsConcept(file) {
		s.onConceptFileRemove(file)
	} else {
		removeWatcherOn(watcher, file)
	}
}

func (s *SpecInfoGatherer) onFileRename(watcher *fsnotify.Watcher, file string) {
	s.onFileRemove(watcher, file)
}

func (s *SpecInfoGatherer) handleEvent(event fsnotify.Event, watcher *fsnotify.Watcher) {
	s.waitGroup.Wait()

	file, err := filepath.Abs(event.Name)
	if err != nil {
		logger.APILog.Error("Failed to get abs file path for %s: %s", event.Name, err)
		return
	}
	if util.IsSpec(file) || util.IsConcept(file) || util.IsDir(file) {
		switch event.Op {
		case fsnotify.Create:
			s.onFileAdd(watcher, file)
		case fsnotify.Write:
			s.onFileModify(watcher, file)
		case fsnotify.Rename:
			s.onFileRename(watcher, file)
		case fsnotify.Remove:
			s.onFileRemove(watcher, file)
		}
	}
}

func (s *SpecInfoGatherer) watchForFileChanges() {
	s.waitGroup.Add(1)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.APILog.Error("Error creating fileWatcher: %s", err)
	}
	defer watcher.Close()

	done := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				s.handleEvent(event, watcher)
			case err := <-watcher.Errors:
				logger.APILog.Error("Error event while watching specs", err)
			}
		}
	}()

	var allDirsToWatch []string
	var specDir string

	for _, dir := range s.SpecDirs {
		specDir = filepath.Join(config.ProjectRoot, dir)
		allDirsToWatch = append(allDirsToWatch, specDir)
		allDirsToWatch = append(allDirsToWatch, util.FindAllNestedDirs(specDir)...)
	}

	for _, dir := range allDirsToWatch {
		addDirToFileWatcher(watcher, dir)
	}
	s.waitGroup.Done()
	<-done
}

// GetAvailableSpecs returns the list of all the specs in the gauge project
func (s *SpecInfoGatherer) GetAvailableSpecDetails(specs []string) []*SpecDetail {
	if len(specs) < 1 {
		specs = []string{common.SpecsDirectoryName}
	}
	specFiles := getSpecFiles(specs)
	s.specsCache.mutex.RLock()
	defer s.specsCache.mutex.RUnlock()
	var details []*SpecDetail
	for _, f := range specFiles {
		if d, ok := s.specsCache.specDetails[f]; ok {
			details = append(details, d)
		}
	}
	return details
}

// GetAvailableSteps returns the list of all the steps in the gauge project
func (s *SpecInfoGatherer) GetAvailableSteps() []*gauge.StepValue {
	var steps []*gauge.StepValue
	s.stepsCache.mutex.RLock()
	defer s.stepsCache.mutex.RUnlock()
	for _, stepValues := range s.stepsCache.stepValues {
		steps = append(steps, stepValues...)
	}
	return steps
}

// GetConceptInfos returns an array containing information about all the concepts present in the Gauge project
func (s *SpecInfoGatherer) GetConceptInfos() []*gauge_messages.ConceptInfo {
	var conceptInfos []*gauge_messages.ConceptInfo
	s.conceptsCache.mutex.RLock()
	defer s.conceptsCache.mutex.RUnlock()
	for _, conceptList := range s.conceptsCache.concepts {
		for _, concept := range conceptList {
			stepValue := parser.CreateStepValue(concept.ConceptStep)
			conceptInfos = append(conceptInfos, &gauge_messages.ConceptInfo{StepValue: gauge.ConvertToProtoStepValue(&stepValue), Filepath: concept.FileName, LineNumber: int32(concept.ConceptStep.LineNo)})
		}
	}
	return conceptInfos
}

func getStepsFromSpec(spec *gauge.Specification) []*gauge.StepValue {
	stepValues := getParsedStepValues(spec.Contexts)
	for _, scenario := range spec.Scenarios {
		stepValues = append(stepValues, getParsedStepValues(scenario.Steps)...)
	}
	return stepValues
}

func getStepsFromConcept(concept *gauge.Concept) []*gauge.StepValue {
	return getParsedStepValues(concept.ConceptStep.ConceptSteps)
}

func getParsedStepValues(steps []*gauge.Step) []*gauge.StepValue {
	var stepValues []*gauge.StepValue
	for _, step := range steps {
		if !step.IsConcept {
			stepValue := parser.CreateStepValue(step)
			stepValues = append(stepValues, &stepValue)
		}
	}
	return stepValues
}

func handleParseFailures(parseResults []*parser.ParseResult) {
	for _, result := range parseResults {
		if !result.Ok {
			logger.APILog.Error("Spec Parse failure: %s", result.Errors())
			if len(result.CriticalErrors) > 0 {
				os.Exit(1)
			}
		}
	}
}

func addDirToFileWatcher(watcher *fsnotify.Watcher, dir string) {
	err := watcher.Add(dir)
	if err != nil {
		logger.APILog.Error("Unable to add directory %v to file watcher: %s", dir, err)
	} else {
		logger.APILog.Info("Watching directory: %s", dir)
		files, _ := ioutil.ReadDir(dir)
		logger.APILog.Debug("Found %d files", len(files))
	}
}

func removeWatcherOn(watcher *fsnotify.Watcher, path string) {
	logger.APILog.Info("Removing watcher on : %s", path)
	watcher.Remove(path)
}
