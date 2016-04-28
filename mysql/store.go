package mysql

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jinzhu/gorm"

	"github.com/olivere/jobqueue"
)

const (
	mysqlSchema = `CREATE TABLE IF NOT EXISTS jobqueue_jobs (
id varchar(36) primary key,
topic varchar(255),
state varchar(30),
args text,
priority bigint,
retry integer,
max_retry integer,
correlation_id varchar(255),
created bigint,
started bigint,
completed bigint,
last_mod bigint,
index ix_jobs_topic (topic),
index ix_jobs_state (state),
index ix_jobs_priority (priority),
index ix_jobs_correlation_id (correlation_id),
index ix_jobs_created (created),
index ix_jobs_started (started),
index ix_jobs_completed (completed),
index ix_jobs_last_mod (last_mod));`
)

// Store represents a persistent MySQL storage implementation.
// It implements the jobqueue.Store interface.
type Store struct {
	db    *gorm.DB
	debug bool
}

// StoreOption is an options provider for Store.
type StoreOption func(*Store)

// NewStore initializes a new MySQL-based storage.
func NewStore(url string, options ...StoreOption) (*Store, error) {
	st := &Store{}
	for _, opt := range options {
		opt(st)
	}
	cfg, err := mysqldriver.ParseDSN(url)
	if err != nil {
		return nil, err
	}
	dbname := cfg.DBName
	if dbname == "" {
		return nil, errors.New("no database specified")
	}
	// First connect without DB name
	cfg.DBName = ""
	setupdb, err := gorm.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	defer setupdb.Close()
	// Create database
	_, err = setupdb.DB().Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", dbname))
	if err != nil {
		return nil, err
	}

	// Now connect again, this time with the db name
	st.db, err = gorm.Open("mysql", url)
	if err != nil {
		return nil, err
	}
	if st.debug {
		st.db = st.db.Debug()
	}
	// Create schema
	_, err = st.db.DB().Exec(mysqlSchema)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// SetDebug indicates whether to enable or disable debugging (which will
// output SQL to the console).
func SetDebug(enabled bool) StoreOption {
	return func(s *Store) {
		s.debug = enabled
	}
}

/*
func SetCleaner(interval, expiry time.Duration) StoreOption {
	return func(s *Store) {
		s.interval = interval
		s.expiry = expiry
	}
}
*/

func (s *Store) wrapError(err error) error {
	if err == gorm.ErrRecordNotFound {
		// Map gorm.ErrRecordNotFound to jobqueue-specific "not found" error
		return jobqueue.ErrNotFound
	}
	return err
}

// Start is called when the manager starts up.
// We ensure that stale jobs are marked as failed so that we have place
// for new jobs.
func (s *Store) Start() error {
	// TODO This will fail if we have two or more job queues working on the same database!
	err := s.db.Model(&Job{}).
		Where("state = ?", jobqueue.Working).
		Updates(map[string]interface{}{
			"state":     jobqueue.Failed,
			"completed": time.Now().UnixNano(),
		}).
		Error
	return s.wrapError(err)
}

// Create adds a new job to the store.
func (s *Store) Create(job *jobqueue.Job) error {
	j, err := newJob(job)
	if err != nil {
		return err
	}
	j.LastMod = j.Created
	return s.wrapError(s.db.Create(j).Error)
}

// Update updates the job in the store.
func (s *Store) Update(job *jobqueue.Job) error {
	j, err := newJob(job)
	if err != nil {
		return err
	}
	j.LastMod = time.Now().UnixNano()
	return s.wrapError(s.db.Save(j).Error)
}

// Next picks the next job to execute, or nil if no executable job is available.
func (s *Store) Next() (*jobqueue.Job, error) {
	var j Job
	err := s.db.Where("state = ?", jobqueue.Waiting).
		Order("priority desc").
		First(&j).
		Error
	if err == gorm.ErrRecordNotFound {
		return nil, jobqueue.ErrNotFound
	}
	if err != nil {
		return nil, s.wrapError(err)
	}
	return j.ToJob()
}

// Delete removes a job from the store.
func (s *Store) Delete(job *jobqueue.Job) error {
	return s.wrapError(s.db.Where("id = ?", job.ID).Delete(&Job{}).Error)
}

// Lookup retrieves a single job in the store.
func (s *Store) Lookup(id string) (*jobqueue.Job, error) {
	var j Job
	err := s.db.Where("id = ?", id).First(&j).Error
	if err != nil {
		return nil, s.wrapError(err)
	}
	job, err := j.ToJob()
	if err != nil {
		return nil, s.wrapError(err)
	}
	return job, nil
}

// List returns a list of all jobs stored in the data store.
func (s *Store) List(request *jobqueue.ListRequest) (*jobqueue.ListResponse, error) {
	rsp := &jobqueue.ListResponse{}

	// Count
	qry := s.db.Model(&Job{})
	if request.State != "" {
		qry = qry.Where("state = ?", request.State)
	}
	err := qry.Count(&rsp.Total).Error
	if err != nil {
		return nil, s.wrapError(err)
	}

	// Find
	qry = s.db.Order("last_mod desc").
		Offset(request.Offset).
		Limit(request.Limit)
	if request.State != "" {
		qry = qry.Where("state = ?", request.State)
	}
	var list []*Job
	err = qry.Find(&list).Error
	if err != nil {
		return nil, s.wrapError(err)
	}
	for _, j := range list {
		job, err := j.ToJob()
		if err != nil {
			return nil, s.wrapError(err)
		}
		rsp.Jobs = append(rsp.Jobs, job)
	}
	return rsp, nil
}

// Stats returns statistics about the jobs in the store.
func (s *Store) Stats() (*jobqueue.Stats, error) {
	stats := new(jobqueue.Stats)
	err := s.db.Model(&Job{}).Where("state = ?", jobqueue.Waiting).Count(&stats.Waiting).Error
	if err != nil {
		return nil, s.wrapError(err)
	}
	err = s.db.Model(&Job{}).Where("state = ?", jobqueue.Working).Count(&stats.Working).Error
	if err != nil {
		return nil, s.wrapError(err)
	}
	err = s.db.Model(&Job{}).Where("state = ?", jobqueue.Succeeded).Count(&stats.Succeeded).Error
	if err != nil {
		return nil, s.wrapError(err)
	}
	err = s.db.Model(&Job{}).Where("state = ?", jobqueue.Failed).Count(&stats.Failed).Error
	if err != nil {
		return nil, s.wrapError(err)
	}
	return stats, nil
}

// -- MySQL-internal representation of a task --

type Job struct {
	ID            string `gorm:"primary_key"`
	Topic         string
	State         string
	Args          sql.NullString
	Priority      int64
	Retry         int
	MaxRetry      int
	CorrelationID sql.NullString
	Created       int64
	Started       int64
	Completed     int64
	LastMod       int64
}

func (Job) TableName() string {
	return "jobqueue_jobs"
}

func newJob(job *jobqueue.Job) (*Job, error) {
	var args string
	if job.Args != nil {
		v, err := json.Marshal(job.Args)
		if err != nil {
			return nil, err
		}
		args = string(v)
	}
	return &Job{
		ID:            job.ID,
		Topic:         job.Topic,
		State:         job.State,
		Args:          sql.NullString{String: args, Valid: args != ""},
		Priority:      job.Priority,
		Retry:         job.Retry,
		MaxRetry:      job.MaxRetry,
		CorrelationID: sql.NullString{String: job.CorrelationID, Valid: job.CorrelationID != ""},
		Created:       job.Created,
		Started:       job.Started,
		Completed:     job.Completed,
	}, nil
}

func (j *Job) ToJob() (*jobqueue.Job, error) {
	var args []interface{}
	if j.Args.Valid && j.Args.String != "" {
		if err := json.Unmarshal([]byte(j.Args.String), &args); err != nil {
			return nil, err
		}
	}
	job := &jobqueue.Job{
		ID:            j.ID,
		Topic:         j.Topic,
		State:         j.State,
		Args:          args,
		Priority:      j.Priority,
		Retry:         j.Retry,
		MaxRetry:      j.MaxRetry,
		CorrelationID: j.CorrelationID.String,
		Created:       j.Created,
		Started:       j.Started,
		Completed:     j.Completed,
	}
	return job, nil
}