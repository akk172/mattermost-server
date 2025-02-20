// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package sqlstore

import (
	"fmt"

	sq "github.com/Masterminds/squirrel"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/store"
)

// dbSelecter is an interface used to enable some internal store methods
// using both transaction and normal queries.
type dbSelecter interface {
	Select(i interface{}, query string, args ...interface{}) error
}

func (s SqlChannelStore) CreateInitialSidebarCategories(userId, teamId string) (*model.OrderedSidebarCategories, error) {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return nil, errors.Wrap(err, "CreateInitialSidebarCategories: begin_transaction")
	}
	defer finalizeTransactionX(transaction)

	if err = s.createInitialSidebarCategoriesT(transaction, userId, teamId); err != nil {
		return nil, errors.Wrap(err, "CreateInitialSidebarCategories: createInitialSidebarCategoriesT")
	}

	oc, err := s.getSidebarCategoriesT(transaction, userId, teamId)
	if err != nil {
		return nil, errors.Wrap(err, "CreateInitialSidebarCategories: getSidebarCategoriesT")
	}

	if err := transaction.Commit(); err != nil {
		return nil, errors.Wrap(err, "CreateInitialSidebarCategories: commit_transaction")
	}

	return oc, nil
}

func (s SqlChannelStore) createInitialSidebarCategoriesT(transaction *sqlxTxWrapper, userId, teamId string) error {
	selectQuery, selectParams, _ := s.getQueryBuilder().
		Select("Type").
		From("SidebarCategories").
		Where(sq.Eq{
			"UserId": userId,
			"TeamId": teamId,
			"Type":   []model.SidebarCategoryType{model.SidebarCategoryFavorites, model.SidebarCategoryChannels, model.SidebarCategoryDirectMessages},
		}).ToSql()

	existingTypes := []model.SidebarCategoryType{}
	err := transaction.Select(&existingTypes, selectQuery, selectParams...)
	if err != nil {
		return errors.Wrap(err, "createInitialSidebarCategoriesT: failed to select existing categories")
	}

	hasCategoryOfType := make(map[model.SidebarCategoryType]bool, len(existingTypes))
	for _, existingType := range existingTypes {
		hasCategoryOfType[existingType] = true
	}

	// Use deterministic IDs for default categories to prevent potentially creating multiple copies of a default category
	favoritesCategoryId := fmt.Sprintf("%s_%s_%s", model.SidebarCategoryFavorites, userId, teamId)
	channelsCategoryId := fmt.Sprintf("%s_%s_%s", model.SidebarCategoryChannels, userId, teamId)
	directMessagesCategoryId := fmt.Sprintf("%s_%s_%s", model.SidebarCategoryDirectMessages, userId, teamId)

	if !hasCategoryOfType[model.SidebarCategoryFavorites] {
		// Create the SidebarChannels first since there's more opportunity for something to fail here
		if err := s.migrateFavoritesToSidebarT(transaction, userId, teamId, favoritesCategoryId); err != nil {
			return errors.Wrap(err, "createInitialSidebarCategoriesT: failed to migrate favorites to sidebar")
		}

		if _, err := transaction.NamedExec(`INSERT INTO
			SidebarCategories(Id, UserId, TeamId, SortOrder, Sorting, Type, DisplayName, Muted, Collapsed)
			VALUES(:Id, :UserId, :TeamId, :SortOrder, :Sorting, :Type, :DisplayName, :Muted, :Collapsed)`, &model.SidebarCategory{
			DisplayName: "Favorites", // This will be retranslated by the client into the user's locale
			Id:          favoritesCategoryId,
			UserId:      userId,
			TeamId:      teamId,
			Sorting:     model.SidebarCategorySortDefault,
			SortOrder:   model.DefaultSidebarSortOrderFavorites,
			Type:        model.SidebarCategoryFavorites,
		}); err != nil {
			return errors.Wrap(err, "createInitialSidebarCategoriesT: failed to insert favorites category")
		}
	}

	if !hasCategoryOfType[model.SidebarCategoryChannels] {
		if _, err := transaction.NamedExec(`INSERT INTO
			SidebarCategories(Id, UserId, TeamId, SortOrder, Sorting, Type, DisplayName, Muted, Collapsed)
			VALUES(:Id, :UserId, :TeamId, :SortOrder, :Sorting, :Type, :DisplayName, :Muted, :Collapsed)`, &model.SidebarCategory{
			DisplayName: "Channels", // This will be retranslated by the client into the user's locale
			Id:          channelsCategoryId,
			UserId:      userId,
			TeamId:      teamId,
			Sorting:     model.SidebarCategorySortDefault,
			SortOrder:   model.DefaultSidebarSortOrderChannels,
			Type:        model.SidebarCategoryChannels,
		}); err != nil {
			return errors.Wrap(err, "createInitialSidebarCategoriesT: failed to insert channels category")
		}
	}

	if !hasCategoryOfType[model.SidebarCategoryDirectMessages] {
		if _, err := transaction.NamedExec(`INSERT INTO
			SidebarCategories(Id, UserId, TeamId, SortOrder, Sorting, Type, DisplayName, Muted, Collapsed)
			VALUES(:Id, :UserId, :TeamId, :SortOrder, :Sorting, :Type, :DisplayName, :Muted, :Collapsed)`, &model.SidebarCategory{
			DisplayName: "Direct Messages", // This will be retranslated by the client into the user's locale
			Id:          directMessagesCategoryId,
			UserId:      userId,
			TeamId:      teamId,
			Sorting:     model.SidebarCategorySortRecent,
			SortOrder:   model.DefaultSidebarSortOrderDMs,
			Type:        model.SidebarCategoryDirectMessages,
		}); err != nil {
			return errors.Wrap(err, "createInitialSidebarCategoriesT: failed to insert direct messages category")
		}
	}

	return nil
}

type userMembership struct {
	UserId     string
	ChannelId  string
	CategoryId string
}

func (s SqlChannelStore) migrateMembershipToSidebar(transaction *sqlxTxWrapper, runningOrder *int64, sql string, args ...interface{}) ([]userMembership, error) {
	memberships := []userMembership{}
	if err := transaction.Select(&memberships, sql, args...); err != nil {
		return nil, err
	}

	for _, favorite := range memberships {
		sql, args, _ := s.getQueryBuilder().
			Insert("SidebarChannels").
			Columns("ChannelId", "UserId", "CategoryId", "SortOrder").
			Values(favorite.ChannelId, favorite.UserId, favorite.CategoryId, *runningOrder).ToSql()

		if _, err := transaction.Exec(sql, args...); err != nil && !IsUniqueConstraintError(err, []string{"UserId", "PRIMARY"}) {
			return nil, err
		}
		*runningOrder = *runningOrder + model.MinimalSidebarSortDistance
	}

	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return memberships, nil
}

func (s SqlChannelStore) migrateFavoritesToSidebarT(transaction *sqlxTxWrapper, userId, teamId, favoritesCategoryId string) error {
	favoritesQuery, favoritesParams, _ := s.getQueryBuilder().
		Select("Preferences.Name").
		From("Preferences").
		Join("Channels on Preferences.Name = Channels.Id").
		Join("ChannelMembers on Preferences.Name = ChannelMembers.ChannelId and Preferences.UserId = ChannelMembers.UserId").
		Where(sq.Eq{
			"Preferences.UserId":   userId,
			"Preferences.Category": model.PreferenceCategoryFavoriteChannel,
			"Preferences.Value":    "true",
		}).
		Where(sq.Or{
			sq.Eq{"Channels.TeamId": teamId},
			sq.Eq{"Channels.TeamId": ""},
		}).
		OrderBy(
			"Channels.DisplayName",
			"Channels.Name ASC",
		).ToSql()

	favoriteChannelIds := []string{}
	if err := transaction.Select(&favoriteChannelIds, favoritesQuery, favoritesParams...); err != nil {
		return errors.Wrap(err, "migrateFavoritesToSidebarT: unable to get favorite channel IDs")
	}

	for i, channelId := range favoriteChannelIds {
		if _, err := transaction.NamedExec(`INSERT INTO
			SidebarChannels(ChannelId, UserId, CategoryId, SortOrder)
			VALUES(:ChannelId, :UserId, :CategoryId, :SortOrder)`, &model.SidebarChannel{
			ChannelId:  channelId,
			CategoryId: favoritesCategoryId,
			UserId:     userId,
			SortOrder:  int64(i * model.MinimalSidebarSortDistance),
		}); err != nil {
			return errors.Wrap(err, "migrateFavoritesToSidebarT: unable to insert SidebarChannel")
		}
	}

	return nil
}

// MigrateFavoritesToSidebarChannels populates the SidebarChannels table by analyzing existing user preferences for favorites
// **IMPORTANT** This function should only be called from the migration task and shouldn't be used by itself
func (s SqlChannelStore) MigrateFavoritesToSidebarChannels(lastUserId string, runningOrder int64) (map[string]interface{}, error) {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return nil, err
	}

	defer finalizeTransactionX(transaction)

	sb := s.
		getQueryBuilder().
		Select("Preferences.UserId", "Preferences.Name AS ChannelId", "SidebarCategories.Id AS CategoryId").
		From("Preferences").
		Where(sq.And{
			sq.Eq{"Preferences.Category": model.PreferenceCategoryFavoriteChannel},
			sq.NotEq{"Preferences.Value": "false"},
			sq.NotEq{"SidebarCategories.Id": nil},
			sq.Gt{"Preferences.UserId": lastUserId},
		}).
		LeftJoin("Channels ON (Channels.Id=Preferences.Name)").
		LeftJoin("SidebarCategories ON (SidebarCategories.UserId=Preferences.UserId AND SidebarCategories.Type='"+string(model.SidebarCategoryFavorites)+"' AND (SidebarCategories.TeamId=Channels.TeamId OR Channels.TeamId=''))").
		OrderBy("Preferences.UserId", "Channels.Name DESC").
		Limit(100)

	sql, args, err := sb.ToSql()
	if err != nil {
		return nil, err
	}

	userFavorites, err := s.migrateMembershipToSidebar(transaction, &runningOrder, sql, args...)
	if err != nil {
		return nil, err
	}
	if len(userFavorites) == 0 {
		return nil, nil
	}

	data := make(map[string]interface{})
	data["UserId"] = userFavorites[len(userFavorites)-1].UserId
	data["SortOrder"] = runningOrder
	return data, nil
}

type sidebarCategoryForJoin struct {
	model.SidebarCategory
	ChannelId *string
}

func (s SqlChannelStore) CreateSidebarCategory(userId, teamId string, newCategory *model.SidebarCategoryWithChannels) (*model.SidebarCategoryWithChannels, error) {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return nil, errors.Wrap(err, "begin_transaction")
	}

	defer finalizeTransactionX(transaction)

	categoriesWithOrder, err := s.getSidebarCategoriesT(transaction, userId, teamId)
	if err != nil {
		return nil, err
	} else if len(categoriesWithOrder.Categories) == 0 {
		return nil, store.NewErrNotFound("categories not found", fmt.Sprintf("userId=%s,teamId=%s", userId, teamId))
	}

	newOrder := categoriesWithOrder.Order
	newCategoryId := model.NewId()
	newCategorySortOrder := 0
	/*
		When a new category is created, it should be placed as follows:
		1. If the Favorites category is first, the new category should be placed after it
		2. Otherwise, the new category should be placed first.
	*/
	if categoriesWithOrder.Categories[0].Type == model.SidebarCategoryFavorites {
		newOrder = append([]string{newOrder[0], newCategoryId}, newOrder[1:]...)
		newCategorySortOrder = model.MinimalSidebarSortDistance
	} else {
		newOrder = append([]string{newCategoryId}, newOrder...)
	}

	category := &model.SidebarCategory{
		DisplayName: newCategory.DisplayName,
		Id:          newCategoryId,
		UserId:      userId,
		TeamId:      teamId,
		Sorting:     model.SidebarCategorySortDefault,
		SortOrder:   int64(model.MinimalSidebarSortDistance * len(newOrder)), // first we place it at the end of the list
		Type:        model.SidebarCategoryCustom,
		Muted:       newCategory.Muted,
	}
	if _, err2 := transaction.NamedExec(`INSERT INTO
			SidebarCategories(Id, UserId, TeamId, SortOrder, Sorting, Type, DisplayName, Muted, Collapsed)
			VALUES(:Id, :UserId, :TeamId, :SortOrder, :Sorting, :Type, :DisplayName, :Muted, :Collapsed)`, category); err2 != nil {
		return nil, errors.Wrap(err2, "failed to save SidebarCategory")
	}

	if len(newCategory.Channels) > 0 {
		placeHolder, channelIdArgs := constructArrayArgs(newCategory.Channels)
		// Remove any channels from their previous categories and add them to the new one
		var deleteQuery string
		if s.DriverName() == model.DatabaseDriverMysql {
			deleteQuery = `
				DELETE
					SidebarChannels
				FROM
					SidebarChannels
				JOIN
					SidebarCategories ON SidebarChannels.CategoryId = SidebarCategories.Id
				WHERE
					SidebarChannels.UserId = ?
					AND SidebarChannels.ChannelId IN ` + placeHolder + `
					AND SidebarCategories.TeamId = ?`
		} else {
			deleteQuery = `
				DELETE FROM
					SidebarChannels
				USING
					SidebarCategories
				WHERE
					SidebarChannels.CategoryId = SidebarCategories.Id
					AND SidebarChannels.UserId = ?
					AND SidebarChannels.ChannelId IN ` + placeHolder + `
					AND SidebarCategories.TeamId = ?`
		}

		args := []interface{}{userId}
		args = append(args, channelIdArgs...)
		args = append(args, teamId)
		_, err = transaction.Exec(deleteQuery, args...)
		if err != nil {
			return nil, errors.Wrap(err, "failed to delete SidebarChannels")
		}

		insertQuery := s.getQueryBuilder().
			Insert("SidebarChannels").
			Columns("ChannelId", "UserId", "CategoryId", "SortOrder")
		for i, channelID := range newCategory.Channels {
			insertQuery = insertQuery.Values(channelID, userId, newCategoryId, int64(i*model.MinimalSidebarSortDistance))
		}
		sql, args, err := insertQuery.ToSql()
		if err != nil {
			return nil, errors.Wrap(err, "InsertSidebarChannels_Tosql")
		}

		if _, err := transaction.Exec(sql, args...); err != nil {
			return nil, errors.Wrap(err, "failed to save SidebarChannels")
		}
	}

	// now we re-order the categories according to the new order
	if err := s.updateSidebarCategoryOrderT(transaction, newOrder); err != nil {
		return nil, err
	}

	if err := transaction.Commit(); err != nil {
		return nil, errors.Wrap(err, "commit_transaction")
	}

	// patch category to return proper sort order
	category.SortOrder = int64(newCategorySortOrder)
	result := &model.SidebarCategoryWithChannels{
		SidebarCategory: *category,
		Channels:        newCategory.Channels,
	}

	return result, nil
}

func (s SqlChannelStore) completePopulatingCategoryChannels(category *model.SidebarCategoryWithChannels) (*model.SidebarCategoryWithChannels, error) {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return nil, errors.Wrap(err, "begin_transaction")
	}
	defer finalizeTransactionX(transaction)

	result, err := s.completePopulatingCategoryChannelsT(transaction, category)
	if err != nil {
		return nil, err
	}

	if err = transaction.Commit(); err != nil {
		return nil, errors.Wrap(err, "commit_transaction")
	}

	return result, nil
}

func (s SqlChannelStore) completePopulatingCategoryChannelsT(db dbSelecter, category *model.SidebarCategoryWithChannels) (*model.SidebarCategoryWithChannels, error) {
	if category.Type == model.SidebarCategoryCustom || category.Type == model.SidebarCategoryFavorites {
		return category, nil
	}

	var channelTypeFilter sq.Sqlizer
	if category.Type == model.SidebarCategoryDirectMessages {
		// any DM/GM channels that aren't in any category should be returned as part of the Direct Messages category
		channelTypeFilter = sq.Eq{"Channels.Type": []model.ChannelType{model.ChannelTypeDirect, model.ChannelTypeGroup}}
	} else if category.Type == model.SidebarCategoryChannels {
		// any public/private channels that are on the current team and aren't in any category should be returned as part of the Channels category
		channelTypeFilter = sq.And{
			sq.Eq{"Channels.Type": []model.ChannelType{model.ChannelTypeOpen, model.ChannelTypePrivate}},
			sq.Eq{"Channels.TeamId": category.TeamId},
		}
	}

	// A subquery that is true if the channel does not have a SidebarChannel entry for the current user on the current team
	doesNotHaveSidebarChannel := sq.Select("1").
		Prefix("NOT EXISTS (").
		From("SidebarChannels").
		Join("SidebarCategories on SidebarChannels.CategoryId=SidebarCategories.Id").
		Where(sq.And{
			sq.Expr("SidebarChannels.ChannelId = ChannelMembers.ChannelId"),
			sq.Eq{"SidebarCategories.UserId": category.UserId},
			sq.Eq{"SidebarCategories.TeamId": category.TeamId},
		}).
		Suffix(")")

	channels := []string{}
	sql, args, err := s.getQueryBuilder().
		Select("Id").
		From("ChannelMembers").
		LeftJoin("Channels ON Channels.Id=ChannelMembers.ChannelId").
		Where(sq.And{
			sq.Eq{"ChannelMembers.UserId": category.UserId},
			channelTypeFilter,
			sq.Eq{"Channels.DeleteAt": 0},
			doesNotHaveSidebarChannel,
		}).
		OrderBy("DisplayName ASC").ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "channel_tosql")
	}

	if err := db.Select(&channels, sql, args...); err != nil {
		return nil, store.NewErrNotFound("ChannelMembers", "<too many fields>")
	}

	category.Channels = append(channels, category.Channels...)
	return category, nil
}

func (s SqlChannelStore) GetSidebarCategory(categoryId string) (*model.SidebarCategoryWithChannels, error) {
	sql, args, err := s.getQueryBuilder().
		Select("SidebarCategories.*", "SidebarChannels.ChannelId").
		From("SidebarCategories").
		LeftJoin("SidebarChannels ON SidebarChannels.CategoryId=SidebarCategories.Id").
		Where(sq.Eq{"SidebarCategories.Id": categoryId}).
		OrderBy("SidebarChannels.SortOrder ASC").ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "sidebar_category_tosql")
	}

	categories := []*sidebarCategoryForJoin{}
	if err = s.GetReplicaX().Select(&categories, sql, args...); err != nil {
		return nil, store.NewErrNotFound("SidebarCategories", categoryId)
	}

	if len(categories) == 0 {
		return nil, store.NewErrNotFound("SidebarCategories", categoryId)
	}

	result := &model.SidebarCategoryWithChannels{
		SidebarCategory: categories[0].SidebarCategory,
		Channels:        make([]string, 0),
	}
	for _, category := range categories {
		if category.ChannelId != nil {
			result.Channels = append(result.Channels, *category.ChannelId)
		}
	}
	return s.completePopulatingCategoryChannels(result)
}

func (s SqlChannelStore) getSidebarCategoriesT(db dbSelecter, userId, teamId string) (*model.OrderedSidebarCategories, error) {
	oc := model.OrderedSidebarCategories{
		Categories: make(model.SidebarCategoriesWithChannels, 0),
		Order:      make([]string, 0),
	}

	categories := []*sidebarCategoryForJoin{}
	query, args, err := s.getQueryBuilder().
		Select("SidebarCategories.*", "SidebarChannels.ChannelId").
		From("SidebarCategories").
		LeftJoin("SidebarChannels ON SidebarChannels.CategoryId=Id").
		Where(sq.And{
			sq.Eq{"SidebarCategories.UserId": userId},
			sq.Eq{"SidebarCategories.TeamId": teamId},
		}).
		OrderBy("SidebarCategories.SortOrder ASC, SidebarChannels.SortOrder ASC").ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "sidebar_categories_tosql")
	}

	if err := db.Select(&categories, query, args...); err != nil {
		return nil, store.NewErrNotFound("SidebarCategories", fmt.Sprintf("userId=%s,teamId=%s", userId, teamId))
	}

	for _, category := range categories {
		var prevCategory *model.SidebarCategoryWithChannels
		for _, existing := range oc.Categories {
			if existing.Id == category.Id {
				prevCategory = existing
				break
			}
		}
		if prevCategory == nil {
			prevCategory = &model.SidebarCategoryWithChannels{
				SidebarCategory: category.SidebarCategory,
				Channels:        make([]string, 0),
			}
			oc.Categories = append(oc.Categories, prevCategory)
			oc.Order = append(oc.Order, category.Id)
		}
		if category.ChannelId != nil {
			prevCategory.Channels = append(prevCategory.Channels, *category.ChannelId)
		}
	}
	for _, category := range oc.Categories {
		if _, err := s.completePopulatingCategoryChannelsT(db, category); err != nil {
			return nil, err
		}
	}

	return &oc, nil
}

func (s SqlChannelStore) GetSidebarCategories(userId, teamId string) (*model.OrderedSidebarCategories, error) {
	return s.getSidebarCategoriesT(s.GetReplicaX(), userId, teamId)
}

func (s SqlChannelStore) GetSidebarCategoryOrder(userId, teamId string) ([]string, error) {
	ids := []string{}

	sql, args, err := s.getQueryBuilder().
		Select("Id").
		From("SidebarCategories").
		Where(sq.And{
			sq.Eq{"UserId": userId},
			sq.Eq{"TeamId": teamId},
		}).
		OrderBy("SidebarCategories.SortOrder ASC").ToSql()

	if err != nil {
		return nil, errors.Wrap(err, "sidebar_category_tosql")
	}

	if err := s.GetReplicaX().Select(&ids, sql, args...); err != nil {
		return nil, store.NewErrNotFound("SidebarCategories", fmt.Sprintf("userId=%s,teamId=%s", userId, teamId))
	}

	return ids, nil
}

func (s SqlChannelStore) updateSidebarCategoryOrderT(transaction *sqlxTxWrapper, categoryOrder []string) error {
	runningOrder := 0
	for _, categoryId := range categoryOrder {
		sql, args, err := s.getQueryBuilder().
			Update("SidebarCategories").
			Set("SortOrder", runningOrder).
			Where(sq.Eq{"Id": categoryId}).ToSql()
		if err != nil {
			return errors.Wrap(err, "updateSidebarCategoryOrderT_Tosql")
		}

		if _, err := transaction.Exec(sql, args...); err != nil {
			return errors.Wrap(err, "Error updating sidebar category order")
		}
		runningOrder += model.MinimalSidebarSortDistance
	}
	return nil
}

func (s SqlChannelStore) UpdateSidebarCategoryOrder(userId, teamId string, categoryOrder []string) error {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return errors.Wrap(err, "begin_transaction")
	}

	defer finalizeTransactionX(transaction)

	// Ensure no invalid categories are included and that no categories are left out
	existingOrder, err := s.GetSidebarCategoryOrder(userId, teamId)
	if err != nil {
		return err
	}

	if len(existingOrder) != len(categoryOrder) {
		return errors.New("cannot update category order, passed list of categories different size than in DB")
	}

	for _, originalCategoryId := range existingOrder {
		found := false
		for _, newCategoryId := range categoryOrder {
			if newCategoryId == originalCategoryId {
				found = true
				break
			}
		}
		if !found {
			return store.NewErrInvalidInput("SidebarCategories", "id", fmt.Sprintf("%v", categoryOrder))
		}
	}

	if err := s.updateSidebarCategoryOrderT(transaction, categoryOrder); err != nil {
		return err
	}

	if err := transaction.Commit(); err != nil {
		return errors.Wrap(err, "commit_transaction")
	}

	return nil
}

//nolint:unparam
func (s SqlChannelStore) UpdateSidebarCategories(userId, teamId string, categories []*model.SidebarCategoryWithChannels) ([]*model.SidebarCategoryWithChannels, []*model.SidebarCategoryWithChannels, error) {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return nil, nil, errors.Wrap(err, "begin_transaction")
	}
	defer finalizeTransactionX(transaction)

	updatedCategories := []*model.SidebarCategoryWithChannels{}
	originalCategories := []*model.SidebarCategoryWithChannels{}
	for _, category := range categories {
		originalCategory, err2 := s.GetSidebarCategory(category.Id)
		if err2 != nil {
			return nil, nil, errors.Wrap(err2, "failed to find SidebarCategories")
		}

		// Copy category to avoid modifying an argument
		updatedCategory := &model.SidebarCategoryWithChannels{
			SidebarCategory: category.SidebarCategory,
		}

		// Prevent any changes to read-only fields of SidebarCategories
		updatedCategory.UserId = originalCategory.UserId
		updatedCategory.TeamId = originalCategory.TeamId
		updatedCategory.SortOrder = originalCategory.SortOrder
		updatedCategory.Type = originalCategory.Type
		updatedCategory.Muted = originalCategory.Muted

		if updatedCategory.Type != model.SidebarCategoryCustom {
			updatedCategory.DisplayName = originalCategory.DisplayName
		}

		if updatedCategory.Type != model.SidebarCategoryDirectMessages {
			updatedCategory.Channels = make([]string, len(category.Channels))
			copy(updatedCategory.Channels, category.Channels)

			updatedCategory.Muted = category.Muted
		}

		// The order in which the queries are executed in the transaction is important.
		// SidebarCategories need to be update first, and then SidebarChannels should be deleted.
		// The net effect remains the same, but it prevents deadlocks from other transactions
		// operating on the tables in reverse order.

		updateQuery, updateParams, _ := s.getQueryBuilder().
			Update("SidebarCategories").
			Set("DisplayName", updatedCategory.DisplayName).
			Set("Sorting", updatedCategory.Sorting).
			Set("Muted", updatedCategory.Muted).
			Set("Collapsed", updatedCategory.Collapsed).
			Where(sq.Eq{"Id": updatedCategory.Id}).ToSql()

		if _, err = transaction.Exec(updateQuery, updateParams...); err != nil {
			return nil, nil, errors.Wrap(err, "failed to update SidebarCategories")
		}

		// if we are updating DM category, it's order can't channel order cannot be changed.
		if category.Type != model.SidebarCategoryDirectMessages {
			// Remove any SidebarChannels entries that were either:
			// - previously in this category (and any ones that are still in the category will be recreated below)
			// - in another category and are being added to this category
			query, args, err2 := s.getQueryBuilder().
				Delete("SidebarChannels").
				Where(
					sq.And{
						sq.Or{
							sq.Eq{"ChannelId": originalCategory.Channels},
							sq.Eq{"ChannelId": updatedCategory.Channels},
						},
						sq.Eq{"CategoryId": category.Id},
					},
				).ToSql()

			if err2 != nil {
				return nil, nil, errors.Wrap(err2, "update_sidebar_catetories_tosql")
			}

			if _, err = transaction.Exec(query, args...); err != nil {
				return nil, nil, errors.Wrap(err, "failed to delete SidebarChannels")
			}

			runningOrder := 0
			insertQuery := s.getQueryBuilder().
				Insert("SidebarChannels").
				Columns("ChannelId", "UserId", "CategoryId", "SortOrder")
			for _, channelID := range category.Channels {
				insertQuery = insertQuery.Values(channelID, userId, category.Id, int64(runningOrder))
				runningOrder += model.MinimalSidebarSortDistance
			}

			if len(category.Channels) > 0 {
				sql, args, err2 := insertQuery.ToSql()
				if err2 != nil {
					return nil, nil, errors.Wrap(err2, "InsertSidebarChannels_Tosql")
				}

				if _, err2 := transaction.Exec(sql, args...); err2 != nil {
					return nil, nil, errors.Wrap(err2, "failed to save SidebarChannels")
				}
			}
		}

		// Update the favorites preferences based on channels moving into or out of the Favorites category for compatibility
		if category.Type == model.SidebarCategoryFavorites {
			// Remove any old favorites
			sql, args, _ := s.getQueryBuilder().Delete("Preferences").Where(
				sq.Eq{
					"UserId":   userId,
					"Name":     originalCategory.Channels,
					"Category": model.PreferenceCategoryFavoriteChannel,
				},
			).ToSql()

			if _, err = transaction.Exec(sql, args...); err != nil {
				return nil, nil, errors.Wrap(err, "failed to delete Preferences")
			}

			// And then add the new ones
			for _, channelID := range category.Channels {
				// This breaks the PreferenceStore abstraction, but it should be safe to assume that everything is a SQL
				// store in this package.
				if err = s.Preference().(*SqlPreferenceStore).save(transaction, &model.Preference{
					Name:     channelID,
					UserId:   userId,
					Category: model.PreferenceCategoryFavoriteChannel,
					Value:    "true",
				}); err != nil {
					return nil, nil, errors.Wrap(err, "failed to save Preference")
				}
			}
		} else {
			// Remove any old favorites that might have been in this category
			query, args, nErr := s.getQueryBuilder().Delete("Preferences").Where(
				sq.Eq{
					"UserId":   userId,
					"Name":     category.Channels,
					"Category": model.PreferenceCategoryFavoriteChannel,
				},
			).ToSql()
			if nErr != nil {
				return nil, nil, errors.Wrap(nErr, "update_sidebar_categories_tosql")
			}

			if _, nErr = transaction.Exec(query, args...); nErr != nil {
				return nil, nil, errors.Wrap(nErr, "failed to delete Preferences")
			}
		}

		updatedCategories = append(updatedCategories, updatedCategory)
		originalCategories = append(originalCategories, originalCategory)
	}

	// Ensure Channels are populated for Channels/Direct Messages category if they change
	for i, updatedCategory := range updatedCategories {
		populated, nErr := s.completePopulatingCategoryChannelsT(transaction, updatedCategory)
		if nErr != nil {
			return nil, nil, nErr
		}

		updatedCategories[i] = populated
	}

	if err = transaction.Commit(); err != nil {
		return nil, nil, errors.Wrap(err, "commit_transaction")
	}

	return updatedCategories, originalCategories, nil
}

// UpdateSidebarChannelsByPreferences is called when the Preference table is being updated to keep SidebarCategories in sync
// At the moment, it's only handling Favorites and NOT DMs/GMs (those will be handled client side)
func (s SqlChannelStore) UpdateSidebarChannelsByPreferences(preferences model.Preferences) error {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return errors.Wrap(err, "UpdateSidebarChannelsByPreferences: begin_transaction")
	}
	defer finalizeTransactionX(transaction)

	for _, preference := range preferences {
		preference := preference

		if preference.Category != model.PreferenceCategoryFavoriteChannel {
			continue
		}

		// if new preference is false - remove the channel from the appropriate sidebar category
		if preference.Value == "false" {
			if err := s.removeSidebarEntriesForPreferenceT(transaction, &preference); err != nil {
				return errors.Wrap(err, "UpdateSidebarChannelsByPreferences: removeSidebarEntriesForPreferenceT")
			}
		} else {
			if err := s.addChannelToFavoritesCategoryT(transaction, &preference); err != nil {
				return errors.Wrap(err, "UpdateSidebarChannelsByPreferences: addChannelToFavoritesCategoryT")
			}
		}
	}

	if err := transaction.Commit(); err != nil {
		return errors.Wrap(err, "UpdateSidebarChannelsByPreferences: commit_transaction")
	}

	return nil
}

func (s SqlChannelStore) removeSidebarEntriesForPreferenceT(transaction *sqlxTxWrapper, preference *model.Preference) error {
	if preference.Category != model.PreferenceCategoryFavoriteChannel {
		return nil
	}

	// Delete any corresponding SidebarChannels entries in a Favorites category corresponding to this preference.
	var query string
	if s.DriverName() == model.DatabaseDriverMysql {
		query = `
			DELETE
				SidebarChannels
			FROM
				SidebarChannels
			JOIN
				SidebarCategories ON SidebarChannels.CategoryId = SidebarCategories.Id
			WHERE
				SidebarChannels.UserId = ?
				AND SidebarChannels.ChannelId = ?
				AND SidebarCategories.Type = ?`
	} else {
		query = `
			DELETE FROM
				SidebarChannels
			USING
				SidebarCategories
			WHERE
				SidebarChannels.CategoryId = SidebarCategories.Id
				AND SidebarChannels.UserId = ?
				AND SidebarChannels.ChannelId = ?
				AND SidebarCategories.Type = ?`
	}

	if _, err := transaction.Exec(query, preference.UserId, preference.Name, model.SidebarCategoryFavorites); err != nil {
		return errors.Wrap(err, "Failed to remove sidebar entries for preference")
	}

	return nil
}

func (s SqlChannelStore) addChannelToFavoritesCategoryT(transaction *sqlxTxWrapper, preference *model.Preference) error {
	if preference.Category != model.PreferenceCategoryFavoriteChannel {
		return nil
	}

	var channel model.Channel
	if err := transaction.Get(&channel, `SELECT * FROM Channels WHERE Id=?`, preference.Name); err != nil {
		return errors.Wrapf(err, "Failed to get favorited channel with id=%s", preference.Name)
	} else if channel.Id == "" {
		return store.NewErrNotFound("Channel", preference.Name)
	}

	// Get the IDs of the Favorites category/categories that the channel needs to be added to
	builder := s.getQueryBuilder().
		Select("SidebarCategories.Id").
		From("SidebarCategories").
		LeftJoin("SidebarChannels on SidebarCategories.Id = SidebarChannels.CategoryId and SidebarChannels.ChannelId = ?", preference.Name).
		Where(sq.Eq{
			"SidebarCategories.UserId": preference.UserId,
			"Type":                     model.SidebarCategoryFavorites,
		}).
		Where("SidebarChannels.ChannelId is null")

	if channel.TeamId != "" {
		builder = builder.Where(sq.Eq{"TeamId": channel.TeamId})
	}

	idsQuery, idsParams, _ := builder.ToSql()

	categoryIds := []string{}
	if err := transaction.Select(&categoryIds, idsQuery, idsParams...); err != nil {
		return errors.Wrap(err, "Failed to get Favorites sidebar categories")
	}

	if len(categoryIds) == 0 {
		// The channel is already in the Favorites category/categories
		return nil
	}

	// For each category ID, insert a row into SidebarChannels with the given channel ID and a SortOrder that's less than
	// all existing SortOrders in the category so that the newly favorited channel comes first
	insertQuery, insertParams, _ := s.getQueryBuilder().
		Insert("SidebarChannels").
		Columns(
			"ChannelId",
			"CategoryId",
			"UserId",
			"SortOrder",
		).
		Select(
			sq.Select().
				Column("? as ChannelId", preference.Name).
				Column("SidebarCategories.Id as CategoryId").
				Column("? as UserId", preference.UserId).
				Column("COALESCE(MIN(SidebarChannels.SortOrder) - 10, 0) as SortOrder").
				From("SidebarCategories").
				LeftJoin("SidebarChannels on SidebarCategories.Id = SidebarChannels.CategoryId").
				Where(sq.Eq{
					"SidebarCategories.Id": categoryIds,
				}).
				GroupBy("SidebarCategories.Id")).ToSql()

	if _, err := transaction.Exec(insertQuery, insertParams...); err != nil {
		return errors.Wrap(err, "Failed to add sidebar entries for favorited channel")
	}

	return nil
}

// DeleteSidebarChannelsByPreferences is called when the Preference table is being updated to keep SidebarCategories in sync
// At the moment, it's only handling Favorites and NOT DMs/GMs (those will be handled client side)
func (s SqlChannelStore) DeleteSidebarChannelsByPreferences(preferences model.Preferences) error {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return errors.Wrap(err, "DeleteSidebarChannelsByPreferences: begin_transaction")
	}
	defer finalizeTransactionX(transaction)

	for _, preference := range preferences {
		preference := preference

		if preference.Category != model.PreferenceCategoryFavoriteChannel {
			continue
		}

		if err := s.removeSidebarEntriesForPreferenceT(transaction, &preference); err != nil {
			return errors.Wrap(err, "DeleteSidebarChannelsByPreferences: removeSidebarEntriesForPreferenceT")
		}
	}

	if err := transaction.Commit(); err != nil {
		return errors.Wrap(err, "DeleteSidebarChannelsByPreferences: commit_transaction")
	}

	return nil
}

//nolint:unparam
func (s SqlChannelStore) UpdateSidebarChannelCategoryOnMove(channel *model.Channel, newTeamId string) error {
	// if channel is being moved, remove it from the categories, since it's possible that there's no matching category in the new team
	if _, err := s.GetMasterX().Exec("DELETE FROM SidebarChannels WHERE ChannelId=?", channel.Id); err != nil {
		return errors.Wrapf(err, "failed to delete SidebarChannels with channelId=%s", channel.Id)
	}
	return nil
}

func (s SqlChannelStore) ClearSidebarOnTeamLeave(userId, teamId string) error {
	// if user leaves the team, clean their team related entries in sidebar channels and categories
	var deleteQuery string
	if s.DriverName() == model.DatabaseDriverMysql {
		deleteQuery = "DELETE SidebarChannels FROM SidebarChannels LEFT JOIN SidebarCategories ON SidebarCategories.Id = SidebarChannels.CategoryId WHERE SidebarCategories.TeamId=? AND SidebarCategories.UserId=?"
	} else {
		deleteQuery = `
			DELETE FROM
				SidebarChannels
			WHERE
				CategoryId IN (
					SELECT
						CategoryId
					FROM
						SidebarChannels,
						SidebarCategories
					WHERE
						SidebarChannels.CategoryId = SidebarCategories.Id
						AND SidebarCategories.TeamId = ?
						AND SidebarChannels.UserId = ?)`
	}
	if _, err := s.GetMasterX().Exec(deleteQuery, teamId, userId); err != nil {
		return errors.Wrap(err, "failed to delete from SidebarChannels")
	}
	if _, err := s.GetMasterX().Exec("DELETE FROM SidebarCategories WHERE SidebarCategories.TeamId = ? AND SidebarCategories.UserId = ?", teamId, userId); err != nil {
		return errors.Wrap(err, "failed to delete from SidebarCategories")
	}
	return nil
}

// DeleteSidebarCategory removes a custom category and moves any channels into it into the Channels and Direct Messages
// categories respectively. Assumes that the provided user ID and team ID match the given category ID.
func (s SqlChannelStore) DeleteSidebarCategory(categoryId string) error {
	transaction, err := s.GetMasterX().Beginx()
	if err != nil {
		return errors.Wrap(err, "begin_transaction")
	}
	defer finalizeTransactionX(transaction)

	// Ensure that we're deleting a custom category
	var category model.SidebarCategory
	if err = transaction.Get(&category, "SELECT * FROM SidebarCategories WHERE Id = ?", categoryId); err != nil {
		return errors.Wrapf(err, "failed to find SidebarCategories with id=%s", categoryId)
	}

	if category.Type != model.SidebarCategoryCustom {
		return store.NewErrInvalidInput("SidebarCategory", "id", categoryId)
	}

	// The order in which the queries are executed in the transaction is important.
	// SidebarCategories need to be deleted first, and then SidebarChannels.
	// The net effect remains the same, but it prevents deadlocks from other transactions
	// operating on the tables in reverse order.

	// Delete the category itself
	query, args, err := s.getQueryBuilder().
		Delete("SidebarCategories").
		Where(sq.Eq{"Id": categoryId}).ToSql()
	if err != nil {
		return errors.Wrap(err, "delete_sidebar_cateory_tosql")
	}
	if _, err = transaction.Exec(query, args...); err != nil {
		return errors.Wrap(err, "failed to delete SidebarCategory")
	}

	// Delete the channels in the category
	query, args, err = s.getQueryBuilder().
		Delete("SidebarChannels").
		Where(sq.Eq{"CategoryId": categoryId}).ToSql()
	if err != nil {
		return errors.Wrap(err, "delete_sidebar_cateory_tosql")
	}
	if _, err = transaction.Exec(query, args...); err != nil {
		return errors.Wrap(err, "failed to delete SidebarChannel")
	}

	if err := transaction.Commit(); err != nil {
		return errors.Wrap(err, "commit_transaction")
	}

	return nil
}
