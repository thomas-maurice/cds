package pipeline

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/lib/pq"

	"github.com/ovh/cds/engine/api/action"
	"github.com/ovh/cds/engine/api/database"
	"github.com/ovh/cds/engine/api/trigger"
	"github.com/ovh/cds/engine/log"
	"github.com/ovh/cds/sdk"
)

var (
	// ErrNoStage when request requires specific stage but it does not exist
	ErrNoStage = fmt.Errorf("cds: stage does not exist")
)

// LoadStage Get a stage from its ID and pipeline ID
func LoadStage(db database.Querier, pipelineID int64, stageID int64) (*sdk.Stage, error) {
	query := `
		SELECT pipeline_stage.id, pipeline_stage.pipeline_id, pipeline_stage.name, pipeline_stage.build_order, pipeline_stage.enabled, pipeline_stage_prerequisite.parameter, pipeline_stage_prerequisite.expected_value
		FROM pipeline_stage
		LEFT OUTER JOIN pipeline_stage_prerequisite ON pipeline_stage_prerequisite.pipeline_stage_id = pipeline_stage.id
		WHERE pipeline_stage.pipeline_id = $1 
		AND pipeline_stage.id = $2;
		`

	var stage sdk.Stage
	stage.Prerequisites = []sdk.Prerequisite{}

	rows, err := db.Query(query, pipelineID, stageID)
	if err == sql.ErrNoRows {
		return nil, ErrNoStage
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var parameter, expectedValue sql.NullString
		rows.Scan(&stage.ID, &stage.PipelineID, &stage.Name, &stage.BuildOrder, &stage.Enabled, &parameter, &expectedValue)
		if parameter.Valid && expectedValue.Valid {
			p := sdk.Prerequisite{
				Parameter:     parameter.String,
				ExpectedValue: expectedValue.String,
			}
			stage.Prerequisites = append(stage.Prerequisites, p)
		}
	}

	return &stage, nil
}

// InsertStage insert given stage into given database
func InsertStage(db database.QueryExecuter, s *sdk.Stage) error {
	s.Enabled = true
	query := `INSERT INTO "pipeline_stage" (pipeline_id, name, build_order, enabled) VALUES($1,$2,$3,$4) RETURNING id`

	if err := db.QueryRow(query, s.PipelineID, s.Name, s.BuildOrder, true).Scan(&s.ID); err != nil {
		return err
	}
	return InsertStagePrequisites(db, s)
}

// InsertStagePrequisites insert prequisite for given stage in database
func InsertStagePrequisites(db database.QueryExecuter, s *sdk.Stage) error {
	if len(s.Prerequisites) > 0 {
		query := "INSERT INTO \"pipeline_stage_prerequisite\"  (pipeline_stage_id, parameter, expected_value) VALUES "
		args := []interface{}{s.ID}
		for i, p := range s.Prerequisites {
			if i > 0 {
				query += ","
			}
			args = append(args, p.Parameter, p.ExpectedValue)
			query += fmt.Sprintf("($1, $%d, $%d)", len(args)-1, len(args))
		}
		query += " RETURNING id"
		var i int
		if err := db.QueryRow(query, args...).Scan(&i); err != nil {
			return err
		}
	}
	return UpdatePipelineLastModified(db, s.PipelineID)
}

// LoadStages Get all stages for the given pipeline
func LoadStages(db *sql.DB, pipelineID int64) ([]sdk.Stage, error) {
	var stages []sdk.Stage

	query := `
		SELECT pipeline_stage.id, pipeline_stage.name, pipeline_stage.enabled, pipeline_stage_prerequisite.parameter, pipeline_stage_prerequisite.expected_value
		FROM pipeline_stage
		LEFT OUTER JOIN pipeline_stage_prerequisite ON pipeline_stage_prerequisite.pipeline_stage_id = pipeline_stage.id
	 	WHERE pipeline_id = $1 
		ORDER BY build_order ASC`

	rows, err := db.Query(query, pipelineID)
	if err != nil {
		return stages, err
	}
	defer rows.Close()

	mapStages := map[int64]*sdk.Stage{}
	stagesPtr := []*sdk.Stage{}

	for rows.Next() {
		var id int64
		var enabled bool
		var name, parameter, expectedValue sql.NullString
		err = rows.Scan(&id, &name, &enabled, &parameter, &expectedValue)
		if err != nil {
			return stages, err
		}

		var stageData = mapStages[id]
		if stageData == nil {
			stageData = &sdk.Stage{
				ID:      id,
				Name:    name.String,
				Enabled: enabled,
			}
			mapStages[id] = stageData
		}

		if parameter.Valid && expectedValue.Valid {
			p := sdk.Prerequisite{
				Parameter:     parameter.String,
				ExpectedValue: expectedValue.String,
			}
			stageData.Prerequisites = append(stageData.Prerequisites, p)
		}
		stagesPtr = append(stagesPtr, stageData)
	}
	for _, s := range stagesPtr {
		stages = append(stages, *s)
	}
	return stages, nil
}

func loadPipelineStage(db database.Querier, p *sdk.Pipeline, args ...FuncArg) error {
	c := structarg{}
	for _, f := range args {
		f(&c)
	}

	query := `
	SELECT  pipeline_stage_R.id as stage_id, pipeline_stage_R.pipeline_id, pipeline_stage_R.name, pipeline_stage_R.last_modified, 
			pipeline_stage_R.build_order, pipeline_stage_R.enabled, pipeline_stage_R.parameter, 
			pipeline_stage_R.expected_value, pipeline_action_R.id as pipeline_action_id, pipeline_action_R.action_id, pipeline_action_R.action_last_modified,
			pipeline_action_R.action_args, pipeline_action_R.action_enabled
	FROM (
		SELECT  pipeline_stage.id, pipeline_stage.pipeline_id, 
				pipeline_stage.name, pipeline_stage.last_modified ,pipeline_stage.build_order, 
				pipeline_stage.enabled, 
				pipeline_stage_prerequisite.parameter, pipeline_stage_prerequisite.expected_value
		FROM pipeline_stage
		LEFT OUTER JOIN pipeline_stage_prerequisite ON pipeline_stage.id = pipeline_stage_prerequisite.pipeline_stage_id
		WHERE pipeline_id = $1
	) as pipeline_stage_R
	LEFT OUTER JOIN (
		SELECT  pipeline_action.id, action.id as action_id, action.name as action_name, action.last_modified as action_last_modified, 
				pipeline_action.args as action_args, pipeline_action.enabled as action_enabled, 
				pipeline_action.pipeline_stage_id
		FROM action
		JOIN pipeline_action ON pipeline_action.action_id = action.id
	) as pipeline_action_R ON pipeline_action_R.pipeline_stage_id = pipeline_stage_R.id
	ORDER BY pipeline_stage_R.build_order, pipeline_action_R.action_name, pipeline_action_R.id ASC`

	rows, err := db.Query(query, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	mapStages := map[int64]*sdk.Stage{}
	mapAllActions := map[int64]*sdk.Action{}
	mapActionsStages := map[int64][]sdk.Action{}
	mapArgs := map[int64][]string{}
	stagesPtr := []*sdk.Stage{}

	for rows.Next() {
		var stageID, pipelineID int64
		var stageBuildOrder int
		var pipelineActionID, actionID sql.NullInt64
		var stageName string
		var stagePrerequisiteParameter, stagePrerequisiteExpectedValue, actionArgs sql.NullString
		var stageEnabled, actionEnabled sql.NullBool
		var stageLastModified, actionLastModified pq.NullTime

		err = rows.Scan(
			&stageID, &pipelineID, &stageName, &stageLastModified,
			&stageBuildOrder, &stageEnabled, &stagePrerequisiteParameter,
			&stagePrerequisiteExpectedValue, &pipelineActionID, &actionID, &actionLastModified,
			&actionArgs, &actionEnabled)
		if err != nil {
			return err
		}

		//Stage
		stageData := mapStages[stageID]
		if stageData == nil {
			stageData = &sdk.Stage{
				ID:           stageID,
				PipelineID:   pipelineID,
				Name:         stageName,
				Enabled:      stageEnabled.Bool,
				BuildOrder:   stageBuildOrder,
				LastModified: stageLastModified.Time.Unix(),
			}
			mapStages[stageID] = stageData
			stagesPtr = append(stagesPtr, stageData)
		}

		//Stage prequisites
		if stagePrerequisiteParameter.Valid && stagePrerequisiteExpectedValue.Valid {
			p := sdk.Prerequisite{
				Parameter:     stagePrerequisiteParameter.String,
				ExpectedValue: stagePrerequisiteExpectedValue.String,
			}
			var found bool
			for i := range stageData.Prerequisites {
				if stageData.Prerequisites[i].Parameter == p.Parameter &&
					stageData.Prerequisites[i].ExpectedValue == p.ExpectedValue {
					found = true
					break
				}
			}
			if !found {
				stageData.Prerequisites = append(stageData.Prerequisites, p)
			}
		}

		//Get actions
		if pipelineActionID.Valid && actionID.Valid && actionArgs.Valid && actionEnabled.Valid && actionLastModified.Valid {
			var a *sdk.Action
			a = mapAllActions[pipelineActionID.Int64]

			if a == nil {
				a = &sdk.Action{
					PipelineActionID: pipelineActionID.Int64,
					ID:               actionID.Int64,
					Enabled:          actionEnabled.Bool,
					LastModified:     actionLastModified.Time.Unix(),
				}
				mapAllActions[pipelineActionID.Int64] = a
				mapActionsStages[stageID] = append(mapActionsStages[stageID], *a)
				mapArgs[stageID] = append(mapArgs[stageID], actionArgs.String)
			}
		}
	}

	//load Pipeline Actions
	for id := range mapStages {
		for index := range mapActionsStages[id] {
			var a *sdk.Action

			/*
				if c.clearsecret {
					a, err = action.LoadActionByID(db, mapActionsStages[id][index].ID, action.WithClearPasswords())
				} else {
					a, err = action.LoadActionByID(db, mapActionsStages[id][index].ID)
				}
			*/
			a, err = action.LoadActionByID(db, mapActionsStages[id][index].ID)
			if err != nil {
				return fmt.Errorf("loadPipelineStage> cannot action.LoadActionByID %d > %s", mapActionsStages[id][index].ID, err)
			}
			a.Enabled = mapActionsStages[id][index].Enabled
			a.PipelineStageID = id
			a.PipelineActionID = mapActionsStages[id][index].PipelineActionID

			var pipelineActionParameter []sdk.Parameter
			var isUpdated bool
			err = json.Unmarshal([]byte(mapArgs[id][index]), &pipelineActionParameter)
			if err != nil {
				return err
			}

			for i := range a.Parameters {
				isUpdated, pipelineActionParameter = updateParamInList(pipelineActionParameter, a.Parameters[i])
				if !isUpdated {
					pipelineActionParameter = append(pipelineActionParameter, a.Parameters[i])
				}
			}
			a.Parameters = pipelineActionParameter
			mapStages[id].Actions = append(mapStages[id].Actions, *a)
		}
	}
	for _, s := range stagesPtr {
		p.Stages = append(p.Stages, *s)
	}

	return nil
}

// UpdateStage update Stage and all its prequisites
func UpdateStage(db database.QueryExecuter, s *sdk.Stage) error {
	query := `UPDATE pipeline_stage SET name=$1, build_order=$2, enabled=$3 WHERE id=$4`
	_, err := db.Exec(query, s.Name, s.BuildOrder, s.Enabled, s.ID)
	if err != nil {
		return err
	}

	//Remove all prequisites
	if err := deleteStagePrerequisites(db, s.ID); err != nil {
		return err
	}

	//Insert all prequisites
	if err := InsertStagePrequisites(db, s); err != nil {
		return err
	}
	return nil
}

// DeleteStageByID Delete stage with associated pipeline action
func DeleteStageByID(tx database.QueryExecuter, s *sdk.Stage, userID int64) error {

	nbOfStages, err := CountStageByPipelineID(tx, s.PipelineID)
	if err != nil {
		return err
	}

	err = DeletePipelineActionByStage(tx, s.ID, userID)
	if err != nil {
		return err
	}

	err = deleteStageByID(tx, s)
	if err != nil {
		return err
	}

	return moveDownStages(tx, s.PipelineID, s.BuildOrder, nbOfStages)
}

func deleteStageByID(tx database.Executer, s *sdk.Stage) error {
	//Delete stage prequisites
	if err := deleteStagePrerequisites(tx, s.ID); err != nil {
		return err
	}

	//Delete stage
	query := `DELETE FROM pipeline_stage WHERE id = $1`
	_, err := tx.Exec(query, s.ID)
	if err != nil {
		return err
	}

	return nil
}

// CountStageByPipelineID Count the number of stages for the given pipeline
func CountStageByPipelineID(db database.Querier, pipelineID int64) (int, error) {
	var countStages int
	query := `SELECT count(id) FROM "pipeline_stage"
	 		  WHERE pipeline_id = $1`
	err := db.QueryRow(query, pipelineID).Scan(&countStages)
	return countStages, err
}

func seleteAllStageID(db database.QueryExecuter, pipelineID int64) ([]int64, error) {
	var stageIDs []int64
	query := `SELECT id FROM "pipeline_stage"
	 		  WHERE pipeline_id = $1`

	rows, err := db.Query(query, pipelineID)
	if err != nil {
		return stageIDs, err
	}
	defer rows.Close()

	for rows.Next() {
		var stageID int64
		err = rows.Scan(&stageID)
		if err != nil {
			return stageIDs, err
		}
		stageIDs = append(stageIDs, stageID)
	}
	return stageIDs, nil
}

func deleteStagePrerequisites(db database.Executer, stageID int64) error {
	log.Debug("deleteStagePrerequisites> delete prequisites for stage %d ", stageID)
	//Delete stage prequisites
	queryDelete := `DELETE FROM pipeline_stage_prerequisite WHERE pipeline_stage_id = $1`
	_, err := db.Exec(queryDelete, strconv.Itoa(int(stageID)))
	return err
}

// DeleteAllStage  Delete all stages from pipeline ID
func DeleteAllStage(db database.QueryExecuter, pipelineID int64, userID int64) error {
	stageIDs, err := seleteAllStageID(db, pipelineID)
	if err != nil {
		return err
	}

	for _, id := range stageIDs {
		err = DeletePipelineActionByStage(db, id, userID)
		if err != nil {
			return err
		}
		//Delete stage prequisites
		if err := deleteStagePrerequisites(db, id); err != nil {
			return err
		}
	}

	queryDelete := `DELETE FROM pipeline_stage WHERE pipeline_id = $1`
	_, err = db.Exec(queryDelete, pipelineID)
	return err
}

// MoveStage Move a stage
func MoveStage(db *sql.DB, stageToMove *sdk.Stage, newBuildOrder int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if stageToMove.BuildOrder > newBuildOrder {
		err = moveUpStages(tx, stageToMove.PipelineID, stageToMove.BuildOrder, newBuildOrder)
		if err != nil {
			return err
		}
	} else if stageToMove.BuildOrder < newBuildOrder {
		err = moveDownStages(tx, stageToMove.PipelineID, stageToMove.BuildOrder, newBuildOrder)
		if err != nil {
			return err
		}
	}

	stageToMove.BuildOrder = newBuildOrder
	err = UpdateStage(tx, stageToMove)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func moveUpStages(db database.Executer, pipelineID int64, oldPosition, newPosition int) error {
	query := `UPDATE pipeline_stage
		  SET build_order=build_order+1
		  WHERE build_order < $1
		  AND build_order >= $2
		  AND pipeline_id = $3`
	_, err := db.Exec(query, oldPosition, newPosition, pipelineID)
	return err
}

func moveDownStages(db database.Executer, pipelineID int64, oldPosition, newPosition int) error {
	query := `UPDATE pipeline_stage
		  SET build_order=build_order-1
		  WHERE build_order <= $1
		  AND build_order > $2
		  AND pipeline_id = $3`
	_, err := db.Exec(query, newPosition, oldPosition, pipelineID)
	return err
}

// CheckPrerequisites verifies that all prerequisite are matched before scheduling
func CheckPrerequisites(s sdk.Stage, pb sdk.PipelineBuild) (bool, error) {
	for {
		replaced := false
		// Now for each trigger parameter
		for _, pbp := range pb.Parameters {
			// Replace placeholders with their value
			for i := range pb.Parameters {
				old := pb.Parameters[i].Value
				pb.Parameters[i].Value = strings.Replace(pb.Parameters[i].Value, "{{."+pbp.Name+"}}", pbp.Value, -1)
				if pb.Parameters[i].Value != old {
					replaced = true
				}
			}
		}
		// If nothing has been replace, exit
		if !replaced {
			break
		}
	}

	// Check conditions
	for _, p := range s.Prerequisites {
		for _, pbp := range pb.Parameters {
			if p.Parameter == pbp.Name {
				//Process expected value as in triggers
				var expectedValue = trigger.ProcessTriggerExpectedValue(p.ExpectedValue, pb)
				//Checking regular expression
				if !strings.HasPrefix(expectedValue, "^") {
					expectedValue = "^" + expectedValue
				}
				if !strings.HasSuffix(expectedValue, "$") {
					expectedValue = expectedValue + "$"
				}
				ok, err := regexp.Match(expectedValue, []byte(pbp.Value))
				if err != nil {
					log.Warning("CheckPrerequisites> Cannot eval regexp '%s': %s", p.ExpectedValue, err)
					return false, fmt.Errorf("CheckPrerequisites> %s", err)
				}
				if !ok {
					log.Debug("CheckPrerequisites> Expected '%s', got '%s'\n", p.ExpectedValue, pbp.Value)
					return false, nil
				}
			}
		}
	}
	return true, nil
}
