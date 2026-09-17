package database

func Migrate() error {
	statements := []string{
		`ALTER TABLE irrigation_zones ADD COLUMN IF NOT EXISTS daily_water_quota DECIMAL(12,2)`,
		`CREATE TABLE IF NOT EXISTS water_reservations (
			id SERIAL PRIMARY KEY,
			schedule_id INTEGER REFERENCES irrigation_schedules(id),
			zone_id INTEGER REFERENCES irrigation_zones(id),
			log_id INTEGER REFERENCES irrigation_logs(id),
			trigger_type trigger_type NOT NULL,
			quota_date DATE NOT NULL,
			requested DECIMAL(12,2) NOT NULL,
			actual_usage DECIMAL(12,2),
			status VARCHAR(20) NOT NULL,
			released_at TIMESTAMP,
			consumed_at TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`ALTER TABLE water_reservations DROP COLUMN IF EXISTS expected_end_at`,
		`CREATE INDEX IF NOT EXISTS idx_water_reservations_zone_day ON water_reservations(zone_id, quota_date)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_water_reservation_log_active
			ON water_reservations(log_id) WHERE status = 'active'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_water_reservation_schedule_day_active
			ON water_reservations(schedule_id, quota_date, trigger_type)
			WHERE schedule_id IS NOT NULL AND status = 'active'`,
		`CREATE TABLE IF NOT EXISTS schedule_postponements (
			id SERIAL PRIMARY KEY,
			schedule_id INTEGER NOT NULL REFERENCES irrigation_schedules(id),
			zone_id INTEGER REFERENCES irrigation_zones(id),
			trigger_type trigger_type NOT NULL,
			quota_date DATE NOT NULL,
			required_amount DECIMAL(12,2) NOT NULL,
			earliest_execute_at TIMESTAMP NOT NULL,
			status VARCHAR(20) NOT NULL,
			reservation_id INTEGER REFERENCES water_reservations(id),
			executed_at TIMESTAMP,
			failed_at TIMESTAMP,
			canceled_at TIMESTAMP,
			reason TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_schedule_postponements_day ON schedule_postponements(schedule_id, quota_date)`,
		`CREATE INDEX IF NOT EXISTS idx_schedule_postponements_execute ON schedule_postponements(status, earliest_execute_at)`,
		`DROP INDEX IF EXISTS uq_schedule_postponement_day_trigger`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_schedule_postponement_day_trigger
			ON schedule_postponements(schedule_id, quota_date, trigger_type)
			WHERE status IN ('pending', 'executing', 'completed')`,
	}

	for _, statement := range statements {
		if err := DB.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
