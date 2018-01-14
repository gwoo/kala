package job

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"

	log "github.com/Sirupsen/logrus"
	"github.com/cornelk/hashmap"
	"github.com/gwoo/kala/metrics"
)

var (
	ErrJobDoesntExist = errors.New("The job you requested does not exist")
)

type JobCache interface {
	Get(id string) (*Job, error)
	GetAll() *JobsMap
	Set(j *Job) error
	Delete(id string) error
	Persist() error
	SetMetrics(*metrics.Metrics)
	GetMetrics() *metrics.Metrics
}

type JobsMap struct {
	Jobs map[string]*Job
	Lock sync.RWMutex
}

func NewJobsMap() *JobsMap {
	return &JobsMap{
		Jobs: map[string]*Job{},
		Lock: sync.RWMutex{},
	}
}

type MemoryJobCache struct {
	// Jobs is a map from Job id's to pointers to the jobs.
	// Used as the main "data store" within this cache implementation.
	jobs    *JobsMap
	jobDB   JobDB
	metrics *metrics.Metrics
}

func NewMemoryJobCache(jobDB JobDB) *MemoryJobCache {
	return &MemoryJobCache{
		jobs:  NewJobsMap(),
		jobDB: jobDB,
	}
}

func (c *MemoryJobCache) GetMetrics() *metrics.Metrics {
	if c.metrics == nil {
		c.metrics = metrics.NewMetrics("")
	}
	return c.metrics
}

func (c *MemoryJobCache) SetMetrics(m *metrics.Metrics) {
	c.metrics = m
}

func (c *MemoryJobCache) Start(persistWaitTime time.Duration) {
	if persistWaitTime == 0 {
		persistWaitTime = 5 * time.Second
	}

	// Prep cache
	allJobs, err := c.jobDB.GetAll()
	if err != nil {
		log.Fatal(err)
	}
	for _, j := range allJobs {
		if j.ShouldStartWaiting() {
			j.StartWaiting(c)
		}
		err = c.Set(j)
		if err != nil {
			log.Errorln(err)
		}
	}

	// Occasionally, save items in cache to db.
	go c.PersistEvery(persistWaitTime)

	// Process-level defer for shutting down the db.
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		s := <-ch
		log.Infof("Process got signal: %s", s)
		log.Infof("Shutting down....")

		// Persist all jobs to database
		c.Persist()

		// Close the database
		c.jobDB.Close()

		os.Exit(0)
	}()
}

func (c *MemoryJobCache) Get(id string) (*Job, error) {
	c.jobs.Lock.RLock()
	defer c.jobs.Lock.RUnlock()

	j := c.jobs.Jobs[id]
	if j == nil {
		return nil, ErrJobDoesntExist
	}

	return j, nil
}

func (c *MemoryJobCache) GetAll() *JobsMap {
	return c.jobs
}

func (c *MemoryJobCache) Set(j *Job) error {
	c.jobs.Lock.Lock()
	defer c.jobs.Lock.Unlock()
	if j == nil {
		return nil
	}
	c.jobs.Jobs[j.Id] = j
	return nil
}

func (c *MemoryJobCache) Delete(id string) error {
	log.Infoln("Lock on delete")
	c.jobs.Lock.Lock()
	defer c.jobs.Lock.Unlock()

	j := c.jobs.Jobs[id]
	if j == nil {
		return ErrJobDoesntExist
	}

	j.Disable()

	go j.DeleteFromParentJobs(c)

	// Remove itself from dependent jobs as a parent job
	// and possibly delete child jobs if they don't have any other parents.
	go j.DeleteFromDependentJobs(c)

	delete(c.jobs.Jobs, id)

	return nil
}

func (c *MemoryJobCache) Persist() error {
	c.jobs.Lock.RLock()
	defer c.jobs.Lock.RUnlock()
	for _, j := range c.jobs.Jobs {
		err := c.jobDB.Save(j)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *MemoryJobCache) PersistEvery(persistWaitTime time.Duration) {
	wait := time.Tick(persistWaitTime)
	var err error
	for {
		<-wait
		err = c.Persist()
		if err != nil {
			log.Errorf("Error occured persisting the database. Err: %s", err)
		}
	}
}

type LockFreeJobCache struct {
	jobs            *hashmap.HashMap
	jobDB           JobDB
	retentionPeriod time.Duration
	metrics         *metrics.Metrics
}

func NewLockFreeJobCache(jobDB JobDB) *LockFreeJobCache {
	return &LockFreeJobCache{
		jobs:            hashmap.New(),
		jobDB:           jobDB,
		retentionPeriod: -1,
	}
}

func (c *LockFreeJobCache) GetMetrics() *metrics.Metrics {
	if c.metrics == nil {
		c.metrics = metrics.NewMetrics("")
	}
	return c.metrics
}

func (c *LockFreeJobCache) SetMetrics(m *metrics.Metrics) {
	c.metrics = m
}

func (c *LockFreeJobCache) Start(persistWaitTime time.Duration, jobstatTtl time.Duration) {
	if persistWaitTime == 0 {
		persistWaitTime = 5 * time.Second
	}

	// Prep cache
	allJobs, err := c.jobDB.GetAll()
	if err != nil {
		log.Fatal(err)
	}
	for _, j := range allJobs {
		if j.Schedule == "" {
			log.Infof("Job %s:%s skipped.", j.Name, j.Id)
			continue
		}
		if j.ShouldStartWaiting() {
			j.StartWaiting(c)
		}
		log.Infof("Job %s:%s added to cache.", j.Name, j.Id)
		err := c.Set(j)
		if err != nil {
			log.Errorln(err)
		}
	}
	// Occasionally, save items in cache to db.
	go c.PersistEvery(persistWaitTime)

	// Run retention every minute to clean up old job stats entries
	if jobstatTtl > 0 {
		c.retentionPeriod = jobstatTtl
		go c.RetainEvery(1 * time.Minute)
	}

	// Process-level defer for shutting down the db.
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		s := <-ch
		log.Infof("Process got signal: %s", s)
		log.Infof("Shutting down....")

		// Persist all jobs to database
		c.Persist()

		// Close the database
		c.jobDB.Close()

		os.Exit(0)
	}()
}

func (c *LockFreeJobCache) Get(id string) (*Job, error) {
	val, exists := c.jobs.GetStringKey(id)
	if val == nil || !exists {
		return nil, ErrJobDoesntExist
	}
	j := (*Job)(val)
	if j == nil {
		return nil, ErrJobDoesntExist
	}
	return j, nil
}

func (c *LockFreeJobCache) GetAll() *JobsMap {
	jm := NewJobsMap()
	for el := range c.jobs.Iter() {
		jm.Jobs[el.Key.(string)] = (*Job)(el.Value)
	}
	return jm
}

func (c *LockFreeJobCache) Set(j *Job) error {
	if j == nil {
		return nil
	}
	c.jobs.Set(j.Id, unsafe.Pointer(j))
	return nil
}

func (c *LockFreeJobCache) Delete(id string) error {
	j, err := c.Get(id)
	if err != nil {
		return err
	}

	j.Disable()
	go j.DeleteFromParentJobs(c)
	// Remove itself from dependent jobs as a parent job
	// and possibly delete child jobs if they don't have any other parents.
	go j.DeleteFromDependentJobs(c)
	log.Infof("Deleting %s", id)
	c.jobs.Del(id)
	return nil
}

func (c *LockFreeJobCache) Persist() error {
	jm := c.GetAll()
	for _, j := range jm.Jobs {
		j.lock.RLock()
		defer j.lock.RUnlock()
		err := c.jobDB.Save(j)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *LockFreeJobCache) PersistEvery(persistWaitTime time.Duration) {
	wait := time.Tick(persistWaitTime)
	var err error
	for {
		<-wait
		err = c.Persist()
		if err != nil {
			log.Errorf("Error occured persisting the database. Err: %s", err)
		}
	}
}

func (c *LockFreeJobCache) locateJobStatsIndexForRetention(stats []*JobStat) (marker int) {
	now := time.Now()
	expiresAt := now.Add(-c.retentionPeriod)
	pos := -1
	for i, el := range stats {
		diff := el.RanAt.Sub(expiresAt)
		if diff < 0 {
			pos = i
		}
	}
	return pos
}

func (c *LockFreeJobCache) Retain() error {
	for el := range c.jobs.Iter() {
		job := (*Job)(el.Value)
		c.compactJobStats(job)
	}
	return nil
}

func (c *LockFreeJobCache) compactJobStats(job *Job) error {
	job.lock.Lock()
	defer job.lock.Unlock()
	pos := c.locateJobStatsIndexForRetention(job.Stats)
	if pos >= 0 {
		log.Infof("JobStats TTL: removing %d items", pos+1)
		tmp := make([]*JobStat, len(job.Stats)-pos-1)
		copy(tmp, job.Stats[pos+1:])
		job.Stats = tmp
	}
	return nil
}

func (c *LockFreeJobCache) RetainEvery(retentionWaitTime time.Duration) {
	wait := time.Tick(retentionWaitTime)
	var err error
	for {
		<-wait
		err = c.Retain()
		if err != nil {
			log.Errorf("Error occured during invoking retention. Err: %s", err)
		}
	}
}
