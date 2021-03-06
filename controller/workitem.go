package controller

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/fabric8-services/fabric8-wit/workitem/link"

	"github.com/fabric8-services/fabric8-wit/ptr"

	"context"

	"github.com/fabric8-services/fabric8-wit/app"
	"github.com/fabric8-services/fabric8-wit/application"
	"github.com/fabric8-services/fabric8-wit/codebase"
	"github.com/fabric8-services/fabric8-wit/errors"
	"github.com/fabric8-services/fabric8-wit/jsonapi"
	"github.com/fabric8-services/fabric8-wit/log"
	"github.com/fabric8-services/fabric8-wit/login"
	"github.com/fabric8-services/fabric8-wit/notification"
	"github.com/fabric8-services/fabric8-wit/rendering"
	"github.com/fabric8-services/fabric8-wit/rest"
	"github.com/fabric8-services/fabric8-wit/space/authz"
	"github.com/fabric8-services/fabric8-wit/workitem"

	"github.com/goadesign/goa"
	errs "github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
)

// Defines the constants to be used in json api "type" attribute
const (
	APIStringTypeUser         = "identities"
	APIStringTypeWorkItem     = "workitems"
	APIStringTypeWorkItemType = "workitemtypes"
	none                      = "none"
)

// WorkitemController implements the workitem resource.
type WorkitemController struct {
	*goa.Controller
	db           application.DB
	config       WorkItemControllerConfig
	notification notification.Channel
}

// WorkItemControllerConfig the config interface for the WorkitemController
type WorkItemControllerConfig interface {
	GetCacheControlWorkItems() string
	GetCacheControlWorkItem() string
}

// NewWorkitemController creates a workitem controller.
func NewWorkitemController(service *goa.Service, db application.DB, config WorkItemControllerConfig) *WorkitemController {
	return NewNotifyingWorkitemController(service, db, &notification.DevNullChannel{}, config)
}

// NewNotifyingWorkitemController creates a workitem controller with notification broadcast.
func NewNotifyingWorkitemController(service *goa.Service, db application.DB, notificationChannel notification.Channel, config WorkItemControllerConfig) *WorkitemController {
	n := notificationChannel
	if n == nil {
		n = &notification.DevNullChannel{}
	}
	return &WorkitemController{
		Controller:   service.NewController("WorkitemController"),
		db:           db,
		notification: n,
		config:       config}
}

// WorkitemCreatorOrSpaceOwner checks if the modifier is space owner or workitem creator
func (c *WorkitemController) WorkitemCreatorOrSpaceOwner(ctx context.Context, spaceID uuid.UUID, creatorID uuid.UUID, editorID uuid.UUID) error {
	// check if workitem editor is same as workitem creator
	if editorID == creatorID {
		return nil
	}
	space, err := c.db.Spaces().Load(ctx, spaceID)
	if err != nil {
		return errors.NewNotFoundError("space", spaceID.String())
	}
	// check if workitem editor is same as space owner
	if space != nil && editorID == space.OwnerID {
		return nil
	}
	return errors.NewForbiddenError("user is not a workitem creator or space owner")
}

// Returns true if the user is the work item creator or space collaborator
func authorizeWorkitemEditor(ctx context.Context, db application.DB, spaceID uuid.UUID, creatorID string, editorID string) (bool, error) {
	if editorID == creatorID {
		return true, nil
	}
	authorized, err := authz.Authorize(ctx, spaceID.String())
	if err != nil {
		return false, errors.NewUnauthorizedError(err.Error())
	}
	return authorized, nil
}

// Update does PATCH workitem
func (c *WorkitemController) Update(ctx *app.UpdateWorkitemContext) error {
	if ctx.Payload == nil || ctx.Payload.Data == nil || ctx.Payload.Data.ID == nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewBadParameterError("missing data.ID element in request", nil))
	}
	currentUserIdentityID, err := login.ContextIdentity(ctx)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError(err.Error()))
	}
	var wi *workitem.WorkItem
	err = application.Transactional(c.db, func(appl application.Application) error {
		wi, err = appl.WorkItems().LoadByID(ctx, *ctx.Payload.Data.ID)
		return err
	})
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	creator := wi.Fields[workitem.SystemCreator]
	if creator == nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, errs.New("work item doesn't have creator")))
	}
	creatorIDStr, ok := creator.(string)
	if !ok {
		return jsonapi.JSONErrorResponse(ctx, errs.Errorf("failed to convert user to string: %+v (%[1]T)", creator))
	}
	creatorID, err := uuid.FromString(creatorIDStr)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	authorized, err := authorizeWorkitemEditor(ctx, c.db, wi.SpaceID, creator.(string), currentUserIdentityID.String())
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	if !authorized {
		return jsonapi.JSONErrorResponse(ctx, errors.NewForbiddenError("user is not authorized to access the space"))
	}

	if ctx.Payload.Data.Relationships != nil && ctx.Payload.Data.Relationships.BaseType != nil &&
		ctx.Payload.Data.Relationships.BaseType.Data != nil && ctx.Payload.Data.Relationships.BaseType.Data.ID != wi.Type {

		err := c.WorkitemCreatorOrSpaceOwner(ctx, wi.SpaceID, creatorID, *currentUserIdentityID)
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, err)
		}
		// Store new values of type and version
		newType := ctx.Payload.Data.Relationships.BaseType
		newVersion := ctx.Payload.Data.Attributes[workitem.SystemVersion]

		// Remove version and base type from payload
		delete(ctx.Payload.Data.Attributes, workitem.SystemVersion)
		ctx.Payload.Data.Relationships.BaseType = nil

		// Ensure we do not have any other change in payload except type change
		if (app.WorkItemRelationships{}) != *ctx.Payload.Data.Relationships || len(ctx.Payload.Data.Attributes) > 0 {
			// Todo(ibrahim) - Change this error to 422 Unprocessable entity
			// error once we have this error in our error package. Please see
			// https://github.com/fabric8-services/fabric8-wit/pull/2202#discussion_r208842063
			return jsonapi.JSONErrorResponse(ctx, errors.NewBadParameterErrorFromString("cannot update type along with other fields"))
		}

		// Restore the original values
		ctx.Payload.Data.Relationships.BaseType = newType
		ctx.Payload.Data.Attributes[workitem.SystemVersion] = newVersion

	}
	var rev *workitem.Revision
	err = application.Transactional(c.db, func(appl application.Application) error {
		// The Number of a work item is not allowed to be changed which is why
		// we overwrite the values with its old value after the work item was
		// converted.
		oldNumber := wi.Number
		err = ConvertJSONAPIToWorkItem(ctx, ctx.Method, appl, *ctx.Payload.Data, wi, wi.Type, wi.SpaceID)
		if err != nil {
			return err
		}
		wi.Number = oldNumber
		wi, rev, err = appl.WorkItems().Save(ctx, wi.SpaceID, *wi, *currentUserIdentityID)
		if err != nil {
			return errs.Wrap(err, "Error updating work item")
		}
		return nil
	})
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	wit, err := c.db.WorkItemTypes().Load(ctx.Context, wi.Type)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, errs.Wrapf(err, "failed to load work item type: %s", wi.Type))
	}
	c.notification.Send(ctx, notification.NewWorkItemUpdated(ctx.Payload.Data.ID.String(), rev.ID))
	converted, err := ConvertWorkItem(ctx.Request, *wit, *wi, workItemIncludeHasChildren(ctx, c.db))
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	resp := &app.WorkItemSingle{
		Data: converted,
		Links: &app.WorkItemLinks{
			Self: buildAbsoluteURL(ctx.Request),
		},
	}
	ctx.ResponseData.Header().Set("Last-Modified", lastModified(*wi))
	return ctx.OK(resp)
}

// Show does GET workitem
func (c *WorkitemController) Show(ctx *app.ShowWorkitemContext) error {
	var wi *workitem.WorkItem
	var wit *workitem.WorkItemType
	err := application.Transactional(c.db, func(appl application.Application) error {
		var err error
		wi, err = appl.WorkItems().LoadByID(ctx, ctx.WiID)
		if err != nil {
			return errs.Wrap(err, fmt.Sprintf("Fail to load work item with id %v", ctx.WiID))
		}
		wit, err = appl.WorkItemTypes().Load(ctx.Context, wi.Type)
		if err != nil {
			return errs.Wrapf(err, "failed to load work item type: %s", wi.Type)
		}
		return nil
	})
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	return ctx.ConditionalRequest(*wi, c.config.GetCacheControlWorkItem, func() error {
		comments := workItemIncludeCommentsAndTotal(ctx, c.db, ctx.WiID)
		hasChildren := workItemIncludeHasChildren(ctx, c.db)
		wi2, err := ConvertWorkItem(ctx.Request, *wit, *wi, comments, hasChildren)
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, err)
		}
		resp := &app.WorkItemSingle{
			Data: wi2,
		}
		return ctx.OK(resp)
	})
}

// Delete does DELETE workitem
func (c *WorkitemController) Delete(ctx *app.DeleteWorkitemContext) error {
	currentUserIdentityID, err := login.ContextIdentity(ctx)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError(err.Error()))
	}

	var wi *workitem.WorkItem
	err = application.Transactional(c.db, func(appl application.Application) error {
		wi, err = appl.WorkItems().LoadByID(ctx, ctx.WiID)
		if err != nil {
			return errs.Wrap(err, fmt.Sprintf("Fail to load work item with id %v", ctx.WiID))
		}
		return nil
	})
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}

	// Check if user is space owner or workitem creator. Only space owner or workitem creator are allowed to delete the workitem.
	creator := wi.Fields[workitem.SystemCreator]
	if creator == nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, errs.New("work item doesn't have creator")))
	}
	creatorIDStr, ok := creator.(string)
	if !ok {
		return jsonapi.JSONErrorResponse(ctx, errs.Errorf("failed to convert user to string: %+v (%[1]T)", creator))
	}
	creatorID, err := uuid.FromString(creatorIDStr)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	err = c.WorkitemCreatorOrSpaceOwner(ctx, wi.SpaceID, creatorID, *currentUserIdentityID)
	if err != nil {
		forbidden, _ := errors.IsForbiddenError(err)
		if forbidden {
			return jsonapi.JSONErrorResponse(ctx, errors.NewForbiddenError("user is not authorized to delete the workitem"))

		}
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	err = application.Transactional(c.db, func(appl application.Application) error {
		if err := appl.WorkItemLinks().DeleteRelatedLinks(ctx, ctx.WiID, *currentUserIdentityID); err != nil {
			return errs.Wrapf(err, "failed to delete work item links related to work item %s", ctx.WiID)
		}
		if err := appl.WorkItems().Delete(ctx, ctx.WiID, *currentUserIdentityID); err != nil {
			return errs.Wrapf(err, "error deleting work item %s", ctx.WiID)
		}
		return nil
	})
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	return ctx.OK([]byte{})
}

// Time is default value if no UpdatedAt field is found
func updatedAt(wi workitem.WorkItem) time.Time {
	var t time.Time
	if ua, ok := wi.Fields[workitem.SystemUpdatedAt]; ok {
		t = ua.(time.Time)
	}
	return t.Truncate(time.Second)
}

func lastModified(wi workitem.WorkItem) string {
	return lastModifiedTime(updatedAt(wi))
}

func lastModifiedTime(t time.Time) string {
	return t.Format(time.RFC1123)
}

func findLastModified(wis []workitem.WorkItem) time.Time {
	var t time.Time
	for _, wi := range wis {
		lm := updatedAt(wi)
		if lm.After(t) {
			t = lm
		}
	}
	return t
}

// ConvertJSONAPIToWorkItem is responsible for converting given WorkItem model object into a
// response resource object by jsonapi.org specifications
func ConvertJSONAPIToWorkItem(ctx context.Context, method string, appl application.Application, source app.WorkItem, target *workitem.WorkItem, witID uuid.UUID, spaceID uuid.UUID) error {
	version, err := getVersion(source.Attributes["version"])
	if err != nil {
		return err
	}
	target.Version = version

	if source.Relationships != nil && source.Relationships.Assignees != nil {
		if source.Relationships.Assignees.Data == nil {
			delete(target.Fields, workitem.SystemAssignees)
		} else {
			var ids []string
			for _, d := range source.Relationships.Assignees.Data {
				assigneeUUID, err := uuid.FromString(*d.ID)
				if err != nil {
					return errors.NewBadParameterError("data.relationships.assignees.data.id", *d.ID)
				}
				if ok := appl.Identities().IsValid(ctx, assigneeUUID); !ok {
					return errors.NewBadParameterError("data.relationships.assignees.data.id", *d.ID)
				}
				ids = append(ids, assigneeUUID.String())
			}
			target.Fields[workitem.SystemAssignees] = ids
		}
	}
	if source.Relationships != nil && source.Relationships.Labels != nil {
		// Pass empty array to remove all lables
		// null is treated as bad param
		if source.Relationships.Labels.Data == nil {
			return errors.NewBadParameterError("data.relationships.labels.data", nil)
		}
		distinctIDs := make(map[string]struct{})
		for _, d := range source.Relationships.Labels.Data {
			labelUUID, err := uuid.FromString(*d.ID)
			if err != nil {
				return errors.NewBadParameterError("data.relationships.labels.data.id", *d.ID)
			}
			if ok := appl.Labels().IsValid(ctx, labelUUID); !ok {
				return errors.NewBadParameterError("data.relationships.labels.data.id", *d.ID)
			}
			if _, ok := distinctIDs[labelUUID.String()]; !ok {
				distinctIDs[labelUUID.String()] = struct{}{}
			}
		}
		ids := make([]string, 0, len(distinctIDs))
		for k := range distinctIDs {
			ids = append(ids, k)
		}
		target.Fields[workitem.SystemLabels] = ids
	}
	if source.Relationships != nil && source.Relationships.Boardcolumns != nil {
		// Pass empty array to remove all boardcolumns
		// null is treated as bad param
		if source.Relationships.Boardcolumns.Data == nil {
			return errors.NewBadParameterError(workitem.SystemBoardcolumns, nil)
		}
		distinctIDs := map[uuid.UUID]struct{}{}
		for _, d := range source.Relationships.Boardcolumns.Data {
			columnUUID, err := uuid.FromString(*d.ID)
			if err != nil {
				return errors.NewBadParameterError(workitem.SystemBoardcolumns, *d.ID)
			}
			/* TODO(michaelkleinhenz): check if columnID is valid
			if ok := appl.Boards().validColumn(ctx, columnUUID); !ok {
				return errors.NewBadParameterError(workitem.SystemBoardcolumns, *d.ID)
			}
			*/
			if _, ok := distinctIDs[columnUUID]; !ok {
				distinctIDs[columnUUID] = struct{}{}
			}
		}
		ids := make([]string, 0, len(distinctIDs))
		for k := range distinctIDs {
			ids = append(ids, k.String())
		}
		target.Fields[workitem.SystemBoardcolumns] = ids
	}
	if source.Relationships != nil {
		if source.Relationships.Iteration == nil || (source.Relationships.Iteration != nil && source.Relationships.Iteration.Data == nil) {
			log.Debug(ctx, map[string]interface{}{
				"wi_id":    target.ID,
				"space_id": spaceID,
			}, "assigning the work item to the root iteration of the space.")
			rootIteration, err := appl.Iterations().Root(ctx, spaceID)
			if err != nil {
				return errors.NewInternalError(ctx, err)
			}
			if method == http.MethodPost {
				target.Fields[workitem.SystemIteration] = rootIteration.ID.String()
			} else if method == http.MethodPatch {
				if source.Relationships.Iteration != nil && source.Relationships.Iteration.Data == nil {
					target.Fields[workitem.SystemIteration] = rootIteration.ID.String()
				}
			}
		} else if source.Relationships.Iteration != nil && source.Relationships.Iteration.Data != nil {
			d := source.Relationships.Iteration.Data
			iterationUUID, err := uuid.FromString(*d.ID)
			if err != nil {
				return errors.NewBadParameterError("data.relationships.iteration.data.id", *d.ID)
			}
			if err := appl.Iterations().CheckExists(ctx, iterationUUID); err != nil {
				return errors.NewNotFoundError("data.relationships.iteration.data.id", *d.ID)
			}
			target.Fields[workitem.SystemIteration] = iterationUUID.String()
		}
	}
	if source.Relationships != nil {
		if source.Relationships.Area == nil || (source.Relationships.Area != nil && source.Relationships.Area.Data == nil) {
			log.Debug(ctx, map[string]interface{}{
				"wi_id":    target.ID,
				"space_id": spaceID,
			}, "assigning the work item to the root area of the space.")
			err := appl.Spaces().CheckExists(ctx, spaceID)
			if err != nil {
				return errors.NewInternalError(ctx, err)
			}
			log.Debug(ctx, map[string]interface{}{
				"space_id": spaceID,
			}, "Loading root area for the space")
			rootArea, err := appl.Areas().Root(ctx, spaceID)
			if err != nil {
				return err
			}
			if method == http.MethodPost {
				target.Fields[workitem.SystemArea] = rootArea.ID.String()
			} else if method == http.MethodPatch {
				if source.Relationships.Area != nil && source.Relationships.Area.Data == nil {
					target.Fields[workitem.SystemArea] = rootArea.ID.String()
				}
			}
		} else if source.Relationships.Area != nil && source.Relationships.Area.Data != nil {
			d := source.Relationships.Area.Data
			areaUUID, err := uuid.FromString(*d.ID)
			if err != nil {
				return errors.NewBadParameterError("data.relationships.area.data.id", *d.ID)
			}
			if err := appl.Areas().CheckExists(ctx, areaUUID); err != nil {
				cause := errs.Cause(err)
				switch cause.(type) {
				case errors.NotFoundError:
					return errors.NewNotFoundError("data.relationships.area.data.id", *d.ID)
				default:
					return errs.Wrapf(err, "unknown error when verifying the area id %s", *d.ID)
				}
			}
			target.Fields[workitem.SystemArea] = areaUUID.String()
		}
	}

	if source.Relationships != nil && source.Relationships.BaseType != nil {
		if source.Relationships.BaseType.Data != nil {
			target.Type = source.Relationships.BaseType.Data.ID
		}
	}

	for key, val := range source.Attributes {
		switch key {
		case workitem.SystemDescription:
			// convert legacy description to markup content
			if m := rendering.NewMarkupContentFromValue(val); m != nil {
				// if no description existed before, set the new one
				if target.Fields[key] == nil {
					target.Fields[key] = *m
				} else {
					// only update the 'description' field in the existing description
					existingDescription := target.Fields[key].(rendering.MarkupContent)
					existingDescription.Content = (*m).Content
					target.Fields[key] = existingDescription
				}
			}
		case workitem.SystemDescriptionMarkup:
			markup := val.(string)
			// if no description existed before, set the markup in a new one
			if target.Fields[workitem.SystemDescription] == nil {
				target.Fields[workitem.SystemDescription] = rendering.MarkupContent{Markup: markup}
			} else {
				// only update the 'description' field in the existing description
				existingDescription := target.Fields[workitem.SystemDescription].(rendering.MarkupContent)
				existingDescription.Markup = markup
				target.Fields[workitem.SystemDescription] = existingDescription
			}
		case workitem.SystemCodebase:
			m, err := codebase.NewCodebaseContentFromValue(val)
			if err != nil {
				return errs.Wrapf(err, "failed to create new codebase from value: %+v", val)
			}
			setupCodebase(appl, m, spaceID)
			target.Fields[key] = *m
		default:
			target.Fields[key] = val
		}
	}
	if description, ok := target.Fields[workitem.SystemDescription].(rendering.MarkupContent); ok {
		// verify the description markup
		if !rendering.IsMarkupSupported(description.Markup) {
			return errors.NewBadParameterError("data.relationships.attributes[system.description].markup", description.Markup)
		}
	}
	return nil
}

// setupCodebase is the link between CodebaseContent & Codebase
// setupCodebase creates a codebase and saves it's ID in CodebaseContent
// for future use
func setupCodebase(appl application.Application, cb *codebase.Content, spaceID uuid.UUID) error {
	if cb.CodebaseID == "" {
		newCodeBase := codebase.Codebase{
			SpaceID: spaceID,
			Type:    "git",
			URL:     cb.Repository,
			StackID: ptr.String("java-centos"),
			//TODO: Think of making stackID dynamic value (from analyzer)
		}
		existingCB, err := appl.Codebases().LoadByRepo(context.Background(), spaceID, cb.Repository)
		if existingCB != nil {
			cb.CodebaseID = existingCB.ID.String()
			return nil
		}
		err = appl.Codebases().Create(context.Background(), &newCodeBase)
		if err != nil {
			return errors.NewInternalError(context.Background(), err)
		}
		cb.CodebaseID = newCodeBase.ID.String()
	}
	return nil
}

func getVersion(version interface{}) (int, error) {
	if version != nil {
		v, err := strconv.Atoi(fmt.Sprintf("%v", version))
		if err != nil {
			return -1, errors.NewBadParameterError("data.attributes.version", version)
		}
		return v, nil
	}
	return -1, nil
}

// ConvertWorkItemsToCSV is responsible for converting given []WorkItem model object into a
// []string object containing a set of CSV formatted data lines and a header line with labels.
// This methods combines all CSV data of all WITs into a single CSV and returns the CSV as well
// as the field headers as a seperate slice
func ConvertWorkItemsToCSV(ctx context.Context, app application.Application, allWits []workitem.WorkItemType, wis []workitem.WorkItem, includeHeaderLine bool) (string, []string, error) {
	if len(wis) == 0 {
		// nothing to do
		return "", []string{}, nil
	}
	// uuidStringCache stores the UUID cache for ID resolvings of the CSV conversion
	uuidStringCache := map[string]string{}
	// csvGrid holds the final CSV in non-serialized form.
	csvGrid := [][]string{}
	// create the global column mapping by iterating over all fields of all WITs
	// we're using seperate []string types as maps are not guaranteed to iterate
	// consistently in the same order; we need to iterate multiple times over
	// the columns with a stable order
	columnKeys := []string{}
	columnLabels := []string{}
	// the wits given should be a unique set, but we make sure we process each wit only once
	// also, we re-use this map as a reference map later
	alreadyProcessedWITs := make(map[uuid.UUID]workitem.WorkItemType)
	for _, wit := range allWits {
		// we only process each WIT once
		if _, ok := alreadyProcessedWITs[wit.ID]; ok {
			continue
		}
		alreadyProcessedWITs[wit.ID] = wit
		// retrieve fields for all WITs
		fieldLabels, fieldKeys, err := extractWorkItemTypeFields(wit)
		if err != nil {
			return "", []string{}, errs.Wrapf(err, "failed to retrieve fields for work item type: %s", wit.ID)
		}
		for idx, fieldLabel := range fieldLabels {
			// check if the field is already in the column mapping
			found := false
			for _, columnKey := range columnKeys {
				if columnKey == fieldKeys[idx] {
					found = true
					break
				}
			}
			if !found {
				// this is a new column, add it to the column mapping
				// the CSV key is not unique and contains the field label
				// there might be collisions due to field labels are not
				// required to be unique, but for the usecase, if there is an equally
				// named field label in a template, it is highly probable that this
				// is intended to go into the same column.
				columnKeys = append(columnKeys, fieldKeys[idx])
				columnLabels = append(columnLabels, fieldLabel)
			}
		}
	}
	// sort the column mapping to provide a consistent output column order
	sortedLabels := make([]string, len(columnLabels))
	sortedKeys := make([]string, len(columnKeys))
	copy(sortedLabels, columnLabels)
	sort.Strings(sortedLabels)
	for idxSorted, sortLabel := range sortedLabels {
		for idx, label := range columnLabels {
			if label == sortLabel {
				sortedKeys[idxSorted] = columnKeys[idx]
				break
			}
		}
	}
	columnLabels = sortedLabels
	columnKeys = sortedKeys
	// add the WIT name manually as the first column
	const witNameKey = "_type"
	columnKeys = append([]string{witNameKey}, columnKeys...)
	columnLabels = append([]string{"_Type"}, columnLabels...)
	// the column mapping keys are the header line for the csv
	if includeHeaderLine {
		headerLine := append([]string{}, columnLabels...)
		csvGrid = append(csvGrid, headerLine)
	}
	// now iterate over the work items and retrieve the values according to the column mapping
	for _, thisWI := range wis {
		wiLine := []string{}
		// for each work item, iterate over the column mapping and retrieve values
		if _, ok := alreadyProcessedWITs[thisWI.Type]; !ok {
			return "", []string{}, errs.Errorf("encountered work item %s with unknown work item type %s", thisWI.ID, thisWI.Type)
		}
		fieldKeyValueMap, err := convertWorkItemFieldValues(ctx, app, &uuidStringCache, alreadyProcessedWITs[thisWI.Type], thisWI)
		if err != nil {
			return "", []string{}, errs.Wrapf(err, "failed to retrieve field values for work item: %s", thisWI.ID)
		}
		for _, columnKey := range columnKeys {
			// if this is the WIT type name key, set it
			if columnKey == witNameKey {
				wiLine = append(wiLine, alreadyProcessedWITs[thisWI.Type].Name)
			} else if fieldValue, ok := fieldKeyValueMap[columnKey]; ok {
				// key exists, this column can be filled from the work item
				wiLine = append(wiLine, fieldValue)
			} else {
				// this column is not available in the work item, add an empty string
				wiLine = append(wiLine, "")
			}
		}
		// finally, add the line to the csv grid
		csvGrid = append(csvGrid, wiLine)
	}
	// create CSV from the [][]string
	buf := new(bytes.Buffer)
	w := csv.NewWriter(buf)
	err := w.WriteAll(csvGrid)
	if err != nil {
		return "", []string{}, errs.Wrapf(err, "failed to serialize to CSV format")
	}
	// done, return serialized result.
	return buf.String(), columnLabels, nil
}

// extractWorkItemTypeFields extracts the field information for a wit; it returns slices for
// field labels and field keys
func extractWorkItemTypeFields(wit workitem.WorkItemType) ([]string, []string, error) {
	fieldLabels := []string{}
	fieldKeys := []string{}
	for fieldKey, fieldDefinition := range wit.Fields {
		// extract the label and key
		fieldLabels = append(fieldLabels, fieldDefinition.Label)
		fieldKeys = append(fieldKeys, fieldKey)
	}
	return fieldLabels, fieldKeys, nil
}

// convertWorkItemFieldValues extracts and converts the wi field values; it returns a map
// that maps field keys to converted field values
func convertWorkItemFieldValues(ctx context.Context, app application.Application, uuidStringCache *map[string]string, wit workitem.WorkItemType, wi workitem.WorkItem) (map[string]string, error) {
	fieldMap := make(map[string]string)
	for fieldKey, fieldDefinition := range wit.Fields {
		// convert the value to a string for the CSV
		fieldValueGeneric := wi.Fields[fieldKey]
		fieldType := fieldDefinition.Type
		fieldValueStrSlice, err := fieldType.ConvertToStringSlice(fieldValueGeneric)
		if err != nil {
			return nil, errs.Wrapf(err, "failed to convert type value to string for field key: %s", fieldKey)
		}
		var convertedValue string
		// now retrieve and, if needed, resolve the id value.
		switch fieldType.(type) {
		case workitem.ListType:
			var converted string
			kind := fieldType.(workitem.ListType).ComponentType.Kind
			delim := ""
			for _, elem := range fieldValueStrSlice {
				elemConvertedValue, err := convertValueToString(ctx, app, uuidStringCache, fieldValueGeneric, []string{elem}, fieldKey, kind)
				if err != nil {
					return nil, errs.Wrapf(err, "failed to convert compound type value to string for field key: %s", fieldKey)
				}
				converted = converted + delim + elemConvertedValue
				delim = ";"
			}
			convertedValue = converted
		case workitem.EnumType:
			kind := fieldType.(workitem.EnumType).BaseType.Kind
			convertedValue, err = convertValueToString(ctx, app, uuidStringCache, fieldValueGeneric, fieldValueStrSlice, fieldKey, kind)
		default:
			// all other Kinds don't need compound resolving.
			kind := fieldType.GetKind()
			convertedValue, err = convertValueToString(ctx, app, uuidStringCache, fieldValueGeneric, fieldValueStrSlice, fieldKey, kind)
		}
		if err != nil {
			return nil, errs.Wrapf(err, "failed to resolve type value to string for field key: %s", fieldKey)
		}
		fieldMap[fieldKey] = convertedValue
	}
	return fieldMap, nil
}

// convertValueToString converts a value to a string. This includes ID resolving if needed.
func convertValueToString(ctx context.Context, app application.Application, uuidStringCache *map[string]string, fieldValueGeneric interface{}, fieldValueStrSlice []string, fieldKey string, kind workitem.Kind) (string, error) {
	if fieldValueGeneric != nil && len(fieldValueStrSlice) == 1 {
		switch kind {
		case workitem.KindUser:
			cachedValue, ok := (*uuidStringCache)[fieldValueStrSlice[0]]
			if ok {
				return cachedValue, nil
			}
			userID, err := uuid.FromString(fieldValueStrSlice[0])
			if err != nil {
				return "", errs.Wrapf(err, "failed to convert user type value to string for field key: %s, value %s", fieldKey, fieldValueStrSlice[0])
			}
			user, err := app.Identities().Load(ctx, userID)
			if err != nil {
				return "", errs.Wrapf(err, "failed to retrieve user for field key: %s", fieldKey)
			}
			(*uuidStringCache)[fieldValueStrSlice[0]] = user.Username
			return user.Username, nil
		case workitem.KindIteration:
			cachedValue, ok := (*uuidStringCache)[fieldValueStrSlice[0]]
			if ok {
				return cachedValue, nil
			}
			iterationID, err := uuid.FromString(fieldValueStrSlice[0])
			if err != nil {
				return "", errs.Wrapf(err, "failed to convert iteration type value to string for field key: %s", fieldKey)
			}
			iteration, err := app.Iterations().Load(ctx, iterationID)
			if err != nil {
				return "", errs.Wrapf(err, "failed to retrieve iteration for field key: %s", fieldKey)
			}
			(*uuidStringCache)[fieldValueStrSlice[0]] = iteration.Name
			return iteration.Name, nil
		case workitem.KindArea:
			cachedValue, ok := (*uuidStringCache)[fieldValueStrSlice[0]]
			if ok {
				return cachedValue, nil
			}
			areaID, err := uuid.FromString(fieldValueStrSlice[0])
			if err != nil {
				return "", errs.Wrapf(err, "failed to convert area type value to string for field key: %s", fieldKey)
			}
			area, err := app.Areas().Load(ctx, areaID)
			if err != nil {
				return "", errs.Wrapf(err, "failed to retrieve area for field key: %s", fieldKey)
			}
			(*uuidStringCache)[fieldValueStrSlice[0]] = area.Name
			return area.Name, nil
		case workitem.KindLabel:
			cachedValue, ok := (*uuidStringCache)[fieldValueStrSlice[0]]
			if ok {
				return cachedValue, nil
			}
			labelID, err := uuid.FromString(fieldValueStrSlice[0])
			if err != nil {
				return "", errs.Wrapf(err, "failed to convert label type value to string for field key: %s", fieldKey)
			}
			label, err := app.Labels().Load(ctx, labelID)
			if err != nil {
				return "", errs.Wrapf(err, "failed to retrieve label for field key: %s", fieldKey)
			}
			(*uuidStringCache)[fieldValueStrSlice[0]] = label.Name
			return label.Name, nil
		default:
			// the default case is also used for KindBoardcolumn as resolving the column is not provided by the
			// factories and the resolved name also has limited use for the exported data.
			return fieldValueStrSlice[0], nil
		}
	} else if len(fieldValueStrSlice) == 1 {
		// the value is nil, we append the returned converted string (which is not nil in this case depending on the baseType!).
		// this is an extra case because we may want to do some prosprocessing for some types here.
		return fieldValueStrSlice[0], nil
	} else {
		// ConvertToStringSlice returned nil/empty array, which is a valid response. We add an empty string in this case.
		return "", nil
	}
}

// WorkItemConvertFunc is a open ended function to add additional links/data/relations to a Comment during
// conversion from internal to API
type WorkItemConvertFunc func(*http.Request, *workitem.WorkItem, *app.WorkItem) error

// ConvertWorkItems is responsible for converting given []WorkItem model object into a
// response resource object by jsonapi.org specifications
func ConvertWorkItems(request *http.Request, wits []workitem.WorkItemType, wis []workitem.WorkItem, additional ...WorkItemConvertFunc) ([]*app.WorkItem, error) {
	ops := []*app.WorkItem{}
	if len(wits) != len(wis) {
		return nil, errs.Errorf("length mismatch of work items (%d) and work item types (%d)", len(wis), len(wits))
	}
	for i := 0; i < len(wis); i++ {
		wi, err := ConvertWorkItem(request, wits[i], wis[i], additional...)
		if err != nil {
			return nil, errs.Wrapf(err, "failed to convert work item: %s", wis[i].ID)
		}
		ops = append(ops, wi)
	}
	return ops, nil
}

// ConvertWorkItem is responsible for converting given WorkItem model object into a
// response resource object by jsonapi.org specifications
func ConvertWorkItem(request *http.Request, wit workitem.WorkItemType, wi workitem.WorkItem, additional ...WorkItemConvertFunc) (*app.WorkItem, error) {
	// construct default values from input WI
	relatedURL := rest.AbsoluteURL(request, app.WorkitemHref(wi.ID))
	labelsRelated := relatedURL + "/labels"
	workItemLinksRelated := relatedURL + "/links"

	op := &app.WorkItem{
		ID:   &wi.ID,
		Type: APIStringTypeWorkItem,
		Attributes: map[string]interface{}{
			workitem.SystemVersion: wi.Version,
			workitem.SystemNumber:  wi.Number,
		},
		Relationships: &app.WorkItemRelationships{
			BaseType: &app.RelationBaseType{
				Data: &app.BaseTypeData{
					ID:   wi.Type,
					Type: APIStringTypeWorkItemType,
				},
				Links: &app.GenericLinks{
					Self: ptr.String(rest.AbsoluteURL(request, app.WorkitemtypeHref(wi.Type))),
				},
			},
			Space: app.NewSpaceRelation(wi.SpaceID, rest.AbsoluteURL(request, app.SpaceHref(wi.SpaceID.String()))),
			WorkItemLinks: &app.RelationGeneric{
				Links: &app.GenericLinks{
					Related: &workItemLinksRelated,
				},
			},
		},
		Links: &app.GenericLinksForWorkItem{
			Self:    &relatedURL,
			Related: &relatedURL,
		},
	}

	// Move fields into Relationships or Attributes as needed
	// TODO(kwk): Loop based on WorkItemType and match against Field.Type instead of directly to field value
	for name, val := range wi.Fields {
		switch name {
		case workitem.SystemAssignees:
			if val != nil {
				userID := val.([]interface{})
				op.Relationships.Assignees = &app.RelationGenericList{
					Data: ConvertUsersSimple(request, userID),
				}
			}
		case workitem.SystemLabels:
			if val != nil {
				labelIDs := val.([]interface{})
				op.Relationships.Labels = &app.RelationGenericList{
					Data: ConvertLabelsSimple(request, labelIDs),
					Links: &app.GenericLinks{
						Related: &labelsRelated,
					},
				}
			}
		case workitem.SystemBoardcolumns:
			if val != nil {
				columnIDs := val.([]interface{})
				op.Relationships.Boardcolumns = &app.RelationGenericList{
					Data: ConvertBoardColumnsSimple(request, columnIDs),
				}
			}
		case workitem.SystemCreator:
			if val != nil {
				userID := val.(string)
				data, links := ConvertUserSimple(request, userID)
				op.Relationships.Creator = &app.RelationGeneric{
					Data:  data,
					Links: links,
				}
			}
		case workitem.SystemIteration:
			if val != nil {
				valStr := val.(string)
				data, links := ConvertIterationSimple(request, valStr)
				op.Relationships.Iteration = &app.RelationGeneric{
					Data:  data,
					Links: links,
				}
			}
		case workitem.SystemArea:
			if val != nil {
				valStr := val.(string)
				data, links := ConvertAreaSimple(request, valStr)
				op.Relationships.Area = &app.RelationGeneric{
					Data:  data,
					Links: links,
				}
			}

		case workitem.SystemTitle:
			// 'HTML escape' the title to prevent script injection
			op.Attributes[name] = html.EscapeString(val.(string))
		case workitem.SystemDescription:
			description := rendering.NewMarkupContentFromValue(val)
			if description != nil {
				op.Attributes[name] = (*description).Content
				op.Attributes[workitem.SystemDescriptionMarkup] = (*description).Markup
				op.Attributes[workitem.SystemDescriptionRendered] = rendering.RenderMarkupToHTML((*description).Content, (*description).Markup)
			}
		case workitem.SystemCodebase:
			if val != nil {
				op.Attributes[name] = val
				// TODO: Following format is TBD and hence commented out
				cb := val.(codebase.Content)
				editURL := rest.AbsoluteURL(request, app.CodebaseHref(cb.CodebaseID)+"/edit")
				op.Links.EditCodebase = &editURL
			}
		default:
			op.Attributes[name] = val
		}
	}
	if op.Relationships.Assignees == nil {
		op.Relationships.Assignees = &app.RelationGenericList{Data: nil}
	}
	if op.Relationships.Iteration == nil {
		op.Relationships.Iteration = &app.RelationGeneric{Data: nil}
	}
	if op.Relationships.Area == nil {
		op.Relationships.Area = &app.RelationGeneric{Data: nil}
	}
	// Always include Comments Link, but optionally use workItemIncludeCommentsAndTotal
	workItemIncludeComments(request, &wi, op)
	workItemIncludeChildren(request, &wi, op)
	workItemIncludeEvents(request, &wi, op)
	for _, add := range additional {
		if err := add(request, &wi, op); err != nil {
			return nil, errs.Wrap(err, "failed to run additional conversion function")
		}
	}
	return op, nil
}

// workItemIncludeHasChildren adds meta information about existing children
func workItemIncludeHasChildren(ctx context.Context, appl application.Application, childLinks ...link.WorkItemLinkList) WorkItemConvertFunc {
	// TODO: Wrap ctx in a Timeout context?
	return func(request *http.Request, wi *workitem.WorkItem, wi2 *app.WorkItem) error {
		var hasChildren bool
		// If we already have information about children inside the child links
		// we can use that before querying the DB.
		if len(childLinks) == 1 {
			for _, l := range childLinks[0] {
				if l.LinkTypeID == link.SystemWorkItemLinkTypeParentChildID && l.SourceID == wi.ID {
					hasChildren = true
				}
			}
		}
		if !hasChildren {
			var err error
			repo := appl.WorkItemLinks()
			if repo != nil {
				hasChildren, err = appl.WorkItemLinks().WorkItemHasChildren(ctx, wi.ID)
				log.Info(ctx, map[string]interface{}{"wi_id": wi.ID}, "Work item has children: %t", hasChildren)
				if err != nil {
					log.Error(ctx, map[string]interface{}{
						"wi_id": wi.ID,
						"err":   err,
					}, "unable to find out if work item has children: %s", wi.ID)
					// enforce to have no children
					hasChildren = false
					return errs.Wrapf(err, "failed to determine if work item %s has children", wi.ID)
				}
			}
		}
		if wi2.Relationships.Children == nil {
			wi2.Relationships.Children = &app.RelationGeneric{}
		}
		wi2.Relationships.Children.Meta = map[string]interface{}{
			"hasChildren": hasChildren,
		}
		return nil
	}
}

// includeParentWorkItem adds the parent of given WI to relationships & included object
func includeParentWorkItem(ctx context.Context, ancestors link.AncestorList, childLinks link.WorkItemLinkList) WorkItemConvertFunc {
	return func(request *http.Request, wi *workitem.WorkItem, wi2 *app.WorkItem) error {
		var parentID *uuid.UUID
		// If we have an ancestry we can lookup the parent in no time.
		if ancestors != nil && len(ancestors) != 0 {
			p := ancestors.GetParentOf(wi.ID)
			if p != nil {
				parentID = &p.ID
			}
		}
		// If no parent ID was found in the ancestor list, see if the child
		// link list contains information to use.
		if parentID == nil && childLinks != nil && len(childLinks) != 0 {
			p := childLinks.GetParentIDOf(wi.ID, link.SystemWorkItemLinkTypeParentChildID)
			if p != uuid.Nil {
				parentID = &p
			}
		}
		if wi2.Relationships.Parent == nil {
			wi2.Relationships.Parent = &app.RelationKindUUID{}
		}
		if parentID != nil {
			if wi2.Relationships.Parent.Data == nil {
				wi2.Relationships.Parent.Data = &app.DataKindUUID{}
			}
			wi2.Relationships.Parent.Data.ID = *parentID
			wi2.Relationships.Parent.Data.Type = APIStringTypeWorkItem
		}
		return nil
	}
}

// ListChildren runs the list action.
func (c *WorkitemController) ListChildren(ctx *app.ListChildrenWorkitemContext) error {
	offset, limit := computePagingLimits(ctx.PageOffset, ctx.PageLimit)
	var result []workitem.WorkItem
	var count int
	var wits []workitem.WorkItemType
	err := application.Transactional(c.db, func(appl application.Application) error {
		var err error
		result, count, err = appl.WorkItemLinks().ListWorkItemChildren(ctx, ctx.WiID, &offset, &limit)
		if err != nil {
			return errs.Wrap(err, "unable to list work item children")
		}
		wits, err = loadWorkItemTypesFromArr(ctx.Context, appl, result)
		if err != nil {
			return errs.Wrap(err, "failed to load the work item types")
		}
		return nil
	})
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	return ctx.ConditionalEntities(result, c.config.GetCacheControlWorkItems, func() error {
		var response app.WorkItemList
		application.Transactional(c.db, func(appl application.Application) error {
			hasChildren := workItemIncludeHasChildren(ctx, appl)
			converted, err := ConvertWorkItems(ctx.Request, wits, result, hasChildren)
			if err != nil {
				return errs.WithStack(err)
			}
			response = app.WorkItemList{
				Links: &app.PagingLinks{},
				Meta:  &app.WorkItemListResponseMeta{TotalCount: count},
				Data:  converted,
			}
			return nil
		})
		setPagingLinks(response.Links, buildAbsoluteURL(ctx.Request), len(result), offset, limit, count)
		return ctx.OK(&response)
	})
}

// workItemIncludeChildren adds relationship about children to workitem (include totalCount)
func workItemIncludeChildren(request *http.Request, wi *workitem.WorkItem, wi2 *app.WorkItem) {
	childrenRelated := rest.AbsoluteURL(request, app.WorkitemHref(wi.ID.String())) + "/children"
	if wi2.Relationships.Children == nil {
		wi2.Relationships.Children = &app.RelationGeneric{}
	}
	wi2.Relationships.Children.Links = &app.GenericLinks{
		Related: &childrenRelated,
	}
}

// workItemIncludeEvents adds relationship about events to workitem (include totalCount)
func workItemIncludeEvents(request *http.Request, wi *workitem.WorkItem, wi2 *app.WorkItem) {
	eventsRelated := rest.AbsoluteURL(request, app.WorkitemHref(wi.ID.String())) + "/events"
	if wi2.Relationships.Events == nil {
		wi2.Relationships.Events = &app.RelationGeneric{}
	}
	wi2.Relationships.Events.Links = &app.GenericLinks{
		Related: &eventsRelated,
	}
}

func loadWorkItemTypesFromArr(ctx context.Context, appl application.Application, wis []workitem.WorkItem) ([]workitem.WorkItemType, error) {
	wits := make([]workitem.WorkItemType, len(wis))
	for idx, wi := range wis {
		wit, err := appl.WorkItemTypes().Load(ctx, wi.Type)
		if err != nil {
			return nil, errs.Wrapf(err, "failed to load the work item type: %s", wi.Type)
		}
		wits[idx] = *wit
	}
	return wits, nil
}

func loadWorkItemTypesFromPtrArr(ctx context.Context, appl application.Application, wis []*workitem.WorkItem) ([]workitem.WorkItemType, error) {
	wits := make([]workitem.WorkItemType, len(wis))
	for idx, wi := range wis {
		wit, err := appl.WorkItemTypes().Load(ctx, wi.Type)
		if err != nil {
			return nil, errs.Wrapf(err, "failed to load the work item type: %s", wi.Type)
		}
		wits[idx] = *wit
	}
	return wits, nil
}
