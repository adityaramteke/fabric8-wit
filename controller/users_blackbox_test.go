package controller_test

import (
	"context"
	"testing"

	"github.com/fabric8-services/fabric8-common/id"
	"github.com/fabric8-services/fabric8-wit/account"
	"github.com/fabric8-services/fabric8-wit/app"
	"github.com/fabric8-services/fabric8-wit/app/test"
	. "github.com/fabric8-services/fabric8-wit/controller"
	"github.com/fabric8-services/fabric8-wit/gormtestsupport"
	"github.com/fabric8-services/fabric8-wit/resource"
	testsupport "github.com/fabric8-services/fabric8-wit/test"

	"github.com/goadesign/goa"
	"github.com/satori/go.uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestUsers(t *testing.T) {
	resource.Require(t, resource.Database)
	suite.Run(t, &TestUsersSuite{DBTestSuite: gormtestsupport.NewDBTestSuite()})
}

type TestUsersSuite struct {
	gormtestsupport.DBTestSuite
	svc          *goa.Service
	controller   *UsersController
	userRepo     account.UserRepository
	identityRepo account.IdentityRepository
}

func (s *TestUsersSuite) SetupSuite() {
	s.DBTestSuite.SetupSuite()
	s.svc = goa.New("test")
	s.controller = NewUsersController(s.svc, s.GormDB, s.Configuration)
	s.userRepo = s.GormDB.Users()
	s.identityRepo = s.GormDB.Identities()
}

func (s *TestUsersSuite) SecuredController(identity account.Identity) (*goa.Service, *UsersController) {
	svc := testsupport.ServiceAsUser("Users-Service", identity)
	return svc, NewUsersController(svc, s.GormDB, s.Configuration)
}

func (s *TestUsersSuite) SecuredServiceAccountController(identity account.Identity) (*goa.Service, *UsersController) {
	svc := testsupport.ServiceAsServiceAccountUser("Users-ServiceAccount-Service", identity)
	return svc, NewUsersController(svc, s.GormDB, s.Configuration)
}

func (s *TestUsersSuite) TestObfuscateUserAsServiceAccountBadRequest() {
	// given
	user := s.createRandomUser("TestObfuscateUserAsServiceAccountBadRequest")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)
	secureService, secureController := s.SecuredServiceAccountController(identity)
	// when
	idAsString := "bad-uuid"
	test.ObfuscateUsersBadRequest(s.T(), secureService.Context, secureService, secureController, idAsString)
}

func (s *TestUsersSuite) TestObfuscateUserAsServiceAccountOK() {
	// given
	user := s.createRandomUser("TestObfuscateUserAsServiceAccountOK")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)
	// when
	secureService, secureController := s.SecuredServiceAccountController(identity)
	test.ObfuscateUsersOK(s.T(), secureService.Context, secureService, secureController, (user.ID).String())
	// then
	obfUser, err := s.userRepo.Load(context.Background(), user.ID)
	require.NoError(s.T(), err)
	obsString := obfUser.FullName
	assert.Equal(s.T(), len(obsString), 12)
	assert.Equal(s.T(), obfUser.Email, obsString+"@mail.com")
	assert.Equal(s.T(), obfUser.FullName, obsString)
	assert.Equal(s.T(), obfUser.ImageURL, obsString)
	assert.Equal(s.T(), obfUser.Bio, obsString)
	assert.Equal(s.T(), obfUser.URL, obsString)
	assert.Equal(s.T(), obfUser.Company, obsString)
	assert.Nil(s.T(), obfUser.ContextInformation)
	obfIdentity, err := s.identityRepo.Load(context.Background(), identity.ID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), obfIdentity.Username, obsString)
	assert.Equal(s.T(), obfIdentity.ProfileURL, &obsString)
}

func (s *TestUsersSuite) TestObfuscateUserAsServiceAccountNotFound() {
	// given
	user := s.createRandomUser("TestObfuscateUserAsServiceAccountNotFound")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)

	// when
	secureService, secureController := s.SecuredServiceAccountController(identity)
	idAsString := uuid.NewV4().String() // will never be found.
	test.ObfuscateUsersNotFound(s.T(), secureService.Context, secureService, secureController, idAsString)

}

func (s *TestUsersSuite) TestObfuscateUserAsServiceAccountUnauthorized() {
	// given
	user := s.createRandomUser("TestObfuscateUserAsSvcAcUnauthorized")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)

	// when
	secureService, secureController := s.SecuredController(identity)

	idAsString := (identity.ID).String()
	test.ObfuscateUsersUnauthorized(s.T(), secureService.Context, secureService, secureController, idAsString)

}

func (s *TestUsersSuite) TestUpdateUserAsServiceAccountUnauthorized() {
	// given
	user := s.createRandomUser("TestUpdateUserAsSvcAcUnauthorized")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)

	// when
	newEmail := "TestUpdateUserOK-" + uuid.NewV4().String() + "@email.com"
	newFullName := "TestUpdateUserOK"
	newImageURL := "http://new.image.io/imageurl"
	newBio := "new bio"
	newProfileURL := "http://new.profile.url/url"
	newCompany := "updateCompany " + uuid.NewV4().String()
	secureService, secureController := s.SecuredController(identity)

	contextInformation := map[string]interface{}{
		"last_visited": "yesterday",
		"space":        "3d6dab8d-f204-42e8-ab29-cdb1c93130ad",
		"rate":         100.00,
		"count":        3,
	}
	updateUsersPayload := createUpdateUsersAsServiceAccountPayload(&newEmail, &newFullName, &newBio, &newImageURL, &newProfileURL, &newCompany, nil, nil, contextInformation)

	idAsString := (identity.ID).String()
	test.UpdateUserAsServiceAccountUsersUnauthorized(s.T(), secureService.Context, secureService, secureController, idAsString, updateUsersPayload)

}

func (s *TestUsersSuite) TestUpdateUserAsServiceAccountBadRequest() {
	// given
	user := s.createRandomUser("TestUpdateUserAsServiceAccountBadRequest")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)

	// when
	newEmail := "TestUpdateUserOK-" + uuid.NewV4().String() + "@email.com"
	newFullName := "TestUpdateUserOK"
	newImageURL := "http://new.image.io/imageurl"
	newBio := "new bio"
	newProfileURL := "http://new.profile.url/url"
	newCompany := "updateCompany " + uuid.NewV4().String()
	secureService, secureController := s.SecuredServiceAccountController(identity)

	contextInformation := map[string]interface{}{
		"last_visited": "yesterday",
		"space":        "3d6dab8d-f204-42e8-ab29-cdb1c93130ad",
		"rate":         100.00,
		"count":        3,
	}
	updateUsersPayload := createUpdateUsersAsServiceAccountPayload(&newEmail, &newFullName, &newBio, &newImageURL, &newProfileURL, &newCompany, nil, nil, contextInformation)

	idAsString := "bad-uuid"
	test.UpdateUserAsServiceAccountUsersBadRequest(s.T(), secureService.Context, secureService, secureController, idAsString, updateUsersPayload)

}

func (s *TestUsersSuite) TestUpdateUserAsServiceAccountOK() {
	// given
	user := s.createRandomUser("TestUpdateUserAsServiceAccountOK")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)

	// when
	user.Email = "TestUpdateUserOK-" + uuid.NewV4().String() + "@email.com"
	user.FullName = "TestUpdateUserOK"
	user.ImageURL = "http://new.image.io/imageurl"
	user.Bio = "new bio"
	user.URL = "http://new.profile.url/url"
	user.Company = "updateCompany " + uuid.NewV4().String()
	secureService, secureController := s.SecuredServiceAccountController(identity)

	contextInformation := map[string]interface{}{
		"last_visited": "yesterday",
		"space":        "3d6dab8d-f204-42e8-ab29-cdb1c93130ad",
		"rate":         100.00,
		"count":        3,
	}
	updateUsersPayload := createUpdateUsersAsServiceAccountPayload(&user.Email, &user.FullName, &user.Bio, &user.ImageURL, &user.URL, &user.Company, nil, nil, contextInformation)
	test.UpdateUserAsServiceAccountUsersOK(s.T(), secureService.Context, secureService, secureController, (identity.ID).String(), updateUsersPayload)
}

func (s *TestUsersSuite) TestUpdateUserAsServiceAccountNotFound() {
	// given
	user := s.createRandomUser("TestUpdateUserAsServiceAccountNotFound")
	identity := s.createRandomIdentity(user, account.KeycloakIDP)

	// when
	newEmail := "TestUpdateUserOK-" + uuid.NewV4().String() + "@email.com"
	newFullName := "TestUpdateUserOK"
	newImageURL := "http://new.image.io/imageurl"
	newBio := "new bio"
	newProfileURL := "http://new.profile.url/url"
	newCompany := "updateCompany " + uuid.NewV4().String()
	secureService, secureController := s.SecuredServiceAccountController(identity)

	contextInformation := map[string]interface{}{
		"last_visited": "yesterday",
		"space":        "3d6dab8d-f204-42e8-ab29-cdb1c93130ad",
		"rate":         100.00,
		"count":        3,
	}
	updateUsersPayload := createUpdateUsersAsServiceAccountPayload(&newEmail, &newFullName, &newBio, &newImageURL, &newProfileURL, &newCompany, nil, nil, contextInformation)

	idAsString := uuid.NewV4().String() // will never be found.
	test.UpdateUserAsServiceAccountUsersNotFound(s.T(), secureService.Context, secureService, secureController, idAsString, updateUsersPayload)

}

func (s *TestUsersSuite) TestCreateUserAsServiceAccountOK() {
	// given
	user := s.createRandomUserObject("TestCreateUserAsServiceAccountOK")
	identity := s.createRandomIdentityObject(user, "KC")

	user.ContextInformation = map[string]interface{}{
		"last_visited": "yesterday",
		"space":        "3d6dab8d-f204-42e8-ab29-cdb1c93130ad",
		"rate":         100.00,
		"count":        3,
	}
	secureService, secureController := s.SecuredServiceAccountController(identity)

	// when
	createUserPayload := createCreateUsersAsServiceAccountPayload(&user.Email, &user.FullName, &user.Bio, &user.ImageURL, &user.URL, &user.Company, &identity.Username, &identity.RegistrationCompleted, user.ContextInformation, user.ID.String())
	test.CreateUserAsServiceAccountUsersOK(s.T(), secureService.Context, secureService, secureController, identity.ID.String(), createUserPayload)
}

func (s *TestUsersSuite) TestCreateUserAsServiceAccountUnAuthorized() {

	// given

	newEmail := "T" + uuid.NewV4().String() + "@email.com"
	newFullName := "TesTCreateUserOK"
	newImageURL := "http://new.image.io/imageurl"
	newBio := "new bio"
	newProfileURL := "http://new.profile.url/url"
	newCompany := "u" + uuid.NewV4().String()
	username := "T" + uuid.NewV4().String()
	secureService, secureController := s.SecuredController(testsupport.TestIdentity)
	registrationCompleted := false
	identityId := uuid.NewV4()
	userID := uuid.NewV4()

	contextInformation := map[string]interface{}{
		"last_visited": "yesterday",
		"space":        "3d6dab8d-f204-42e8-ab29-cdb1c93130ad",
		"rate":         100.00,
		"count":        3,
	}

	// then
	createUserPayload := createCreateUsersAsServiceAccountPayload(&newEmail, &newFullName, &newBio, &newImageURL, &newProfileURL, &newCompany, &username, &registrationCompleted, contextInformation, userID.String())
	test.CreateUserAsServiceAccountUsersUnauthorized(s.T(), secureService.Context, secureService, secureController, identityId.String(), createUserPayload)
}

func (s *TestUsersSuite) TestCreateUserAsServiceAccountBadRequest() {

	// given

	newEmail := "T" + uuid.NewV4().String() + "@email.com"
	newFullName := "TesTCreateUserOK"
	newImageURL := "http://new.image.io/imageurl"
	newBio := "new bio"
	newProfileURL := "http://new.profile.url/url"
	newCompany := "u" + uuid.NewV4().String()
	username := "T" + uuid.NewV4().String()
	secureService, secureController := s.SecuredServiceAccountController(testsupport.TestIdentity)
	registrationCompleted := false
	userID := uuid.NewV4()

	contextInformation := map[string]interface{}{
		"last_visited": "yesterday",
		"space":        "3d6dab8d-f204-42e8-ab29-cdb1c93130ad",
		"rate":         100.00,
		"count":        3,
	}

	createUserPayload := createCreateUsersAsServiceAccountPayload(&newEmail, &newFullName, &newBio, &newImageURL, &newProfileURL, &newCompany, &username, &registrationCompleted, contextInformation, userID.String())

	// then
	test.CreateUserAsServiceAccountUsersBadRequest(s.T(), secureService.Context, secureService, secureController, "invalid-uuid", createUserPayload)
}

func (s *TestUsersSuite) createRandomUser(fullname string) account.User {
	user := account.User{
		Email:    uuid.NewV4().String() + "primaryForUpdat7e@example.com",
		FullName: fullname,
		ImageURL: "someURLForUpdate",
		ID:       uuid.NewV4(),
		Company:  uuid.NewV4().String() + "company",
	}
	err := s.userRepo.Create(context.Background(), &user)
	require.NoError(s.T(), err)
	return user
}

func (s *TestUsersSuite) createRandomUserObject(fullname string) account.User {
	user := account.User{
		Email:    uuid.NewV4().String() + "primaryForUpdat7e@example.com",
		FullName: fullname,
		ImageURL: "someURLForUpdate",
		ID:       uuid.NewV4(),
		Company:  uuid.NewV4().String() + "company",
	}
	return user
}
func (s *TestUsersSuite) createRandomIdentityObject(user account.User, providerType string) account.Identity {
	profile := "foobarforupdate.com/" + uuid.NewV4().String() + "/" + user.ID.String()
	identity := account.Identity{
		Username:     "TestUpdateUserIntegration123" + uuid.NewV4().String(),
		ProviderType: providerType,
		ProfileURL:   &profile,
		User:         user,
		UserID:       id.NullUUID{UUID: user.ID, Valid: true},
	}
	return identity
}

func (s *TestUsersSuite) createRandomIdentity(user account.User, providerType string) account.Identity {
	profile := "foobarforupdate.com/" + uuid.NewV4().String() + "/" + user.ID.String()
	identity := account.Identity{
		Username:     "TestUpdateUserIntegration123" + uuid.NewV4().String(),
		ProviderType: providerType,
		ProfileURL:   &profile,
		User:         user,
		UserID:       id.NullUUID{UUID: user.ID, Valid: true},
	}
	err := s.identityRepo.Create(context.Background(), &identity)
	require.NoError(s.T(), err)
	return identity
}

func createUpdateUsersAsServiceAccountPayload(email, fullName, bio, imageURL, profileURL, company, username *string, registrationCompleted *bool, contextInformation map[string]interface{}) *app.UpdateUserAsServiceAccountUsersPayload {
	return &app.UpdateUserAsServiceAccountUsersPayload{
		Data: &app.UpdateUserData{
			Type: "identities",
			Attributes: &app.UpdateIdentityDataAttributes{
				Email:                 email,
				FullName:              fullName,
				Bio:                   bio,
				ImageURL:              imageURL,
				URL:                   profileURL,
				Company:               company,
				ContextInformation:    contextInformation,
				Username:              username,
				RegistrationCompleted: registrationCompleted,
			},
		},
	}
}

func createCreateUsersAsServiceAccountPayload(email, fullName, bio, imageURL, profileURL, company, username *string, registrationCompleted *bool, contextInformation map[string]interface{}, userID string) *app.CreateUserAsServiceAccountUsersPayload {

	return &app.CreateUserAsServiceAccountUsersPayload{
		Data: &app.CreateUserData{
			Type: "identities",
			Attributes: &app.CreateIdentityDataAttributes{
				UserID:                userID,
				Email:                 *email,
				FullName:              fullName,
				Bio:                   bio,
				ImageURL:              imageURL,
				URL:                   profileURL,
				Company:               company,
				ContextInformation:    contextInformation,
				Username:              *username,
				RegistrationCompleted: registrationCompleted,
				ProviderType:          "KC",
			},
		},
	}
}
