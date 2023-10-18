package initialisation

import (
	"context"
	"fmt"
	"log"

	"github.com/spf13/viper"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/pipe-fittings/constants"
	"github.com/turbot/pipe-fittings/db/db_client"
	"github.com/turbot/pipe-fittings/db/db_common"
	"github.com/turbot/pipe-fittings/error_helpers"
	"github.com/turbot/pipe-fittings/modconfig"
	"github.com/turbot/pipe-fittings/modinstaller"
	"github.com/turbot/pipe-fittings/workspace"
	internal_constants "github.com/turbot/powerpipe/internal/constants"
	"github.com/turbot/powerpipe/pkg/export"
	"github.com/turbot/steampipe-plugin-sdk/v5/sperr"
	"github.com/turbot/steampipe-plugin-sdk/v5/telemetry"
	"github.com/turbot/steampipe/pkg/statushooks"
)

type InitData struct {
	Workspace *workspace.Workspace
	Client    db_common.Client
	Result    *db_common.InitResult

	ShutdownTelemetry func()
	ExportManager     *export.Manager
}

func NewErrorInitData(err error) *InitData {
	return &InitData{
		Result: &db_common.InitResult{Error: err},
	}
}

func NewInitData() *InitData {
	i := &InitData{
		Result:        &db_common.InitResult{},
		ExportManager: export.NewManager(),
	}

	return i
}

func (i *InitData) RegisterExporters(exporters ...export.Exporter) *InitData {
	for _, e := range exporters {
		i.ExportManager.Register(e)
	}

	return i
}

func (i *InitData) Init(ctx context.Context, _ constants.Invoker, opts ...db_client.ClientOption) {
	defer func() {
		if r := recover(); r != nil {
			i.Result.Error = helpers.ToError(r)
		}
		// if there is no error, return context cancellation error (if any)
		if i.Result.Error == nil {
			i.Result.Error = ctx.Err()
		}
	}()

	log.Printf("[INFO] Initializing...")

	// code after this depends of i.Workspace being defined. make sure that it is
	if i.Workspace == nil {
		i.Result.Error = sperr.WrapWithRootMessage(error_helpers.InvalidStateError, "InitData.Init called before setting up Workspace")
		return
	}

	statushooks.SetStatus(ctx, "Initializing")

	// initialise telemetry
	shutdownTelemetry, err := telemetry.Init(internal_constants.AppName)
	if err != nil {
		i.Result.AddWarnings(err.Error())
	} else {
		i.ShutdownTelemetry = shutdownTelemetry
	}

	// install mod dependencies if needed
	if viper.GetBool(constants.ArgModInstall) {
		statushooks.SetStatus(ctx, "Installing workspace dependencies")
		log.Printf("[INFO] Installing workspace dependencies")

		opts := modinstaller.NewInstallOpts(i.Workspace.Mod)
		// use force install so that errors are ignored during installation
		// (we are validating prereqs later)
		opts.Force = true
		_, err := modinstaller.InstallWorkspaceDependencies(ctx, opts)
		if err != nil {
			i.Result.Error = err
			return
		}
	}

	// TODO KAI FIX ME
	// retrieve cloud metadata
	//cloudMetadata, err := getCloudMetadata(ctx)
	//if err != nil {
	//	i.Result.Error = err
	//	return
	//}

	// set cloud metadata (may be nil)
	//i.Workspace.CloudMetadata = cloudMetadata

	// get a client
	// add a message rendering function to the context - this is used for the fdw update message and
	// allows us to render it as a standard initialisation message
	getClientCtx := statushooks.AddMessageRendererToContext(ctx, func(format string, a ...any) {
		i.Result.AddMessage(fmt.Sprintf(format, a...))
	})

	statushooks.SetStatus(ctx, "Connecting to steampipe database")
	log.Printf("[INFO] Connecting to steampipe database")
	client, errorsAndWarnings := GetDbClient(getClientCtx, nil, opts...)
	if errorsAndWarnings.Error != nil {
		i.Result.Error = errorsAndWarnings.Error
		return
	}

	i.Result.AddWarnings(errorsAndWarnings.Warnings...)

	log.Printf("[INFO] ValidateClientCacheSettings")
	if errorsAndWarnings := db_common.ValidateClientCacheSettings(client); errorsAndWarnings != nil {
		if errorsAndWarnings.GetError() != nil {
			i.Result.Error = errorsAndWarnings.GetError()
		}
		i.Result.AddWarnings(errorsAndWarnings.Warnings...)
	}

	i.Client = client
}

func validateModRequirementsRecursively(mod *modconfig.Mod, pluginVersionMap map[string]*modconfig.PluginVersionString) []string {
	var validationErrors []string

	// validate this mod
	for _, err := range mod.ValidateRequirements(pluginVersionMap) {
		validationErrors = append(validationErrors, err.Error())
	}

	// validate dependent mods
	for childDependencyName, childMod := range mod.ResourceMaps.Mods {
		// TODO : The 'mod.DependencyName == childMod.DependencyName' check has to be done because
		// of a bug in the resource loading code which also puts the mod itself into the resource map
		// [https://github.com/turbot/steampipe/issues/3341]
		if childDependencyName == "local" || mod.DependencyName == childMod.DependencyName {
			// this is a reference to self - skip (otherwise we will end up with a recursion loop)
			continue
		}
		childValidationErrors := validateModRequirementsRecursively(childMod, pluginVersionMap)
		validationErrors = append(validationErrors, childValidationErrors...)
	}

	return validationErrors
}

// GetDbClient either creates a DB client using the configured connection string (if present) or creates a LocalDbClient
func GetDbClient(ctx context.Context, onConnectionCallback db_client.DbConnectionCallback, opts ...db_client.ClientOption) (db_common.Client, *error_helpers.ErrorAndWarnings) {
	connectionString := viper.GetString(constants.ArgConnectionString)
	if connectionString == "" {
		return nil, error_helpers.NewErrorsAndWarning(sperr.New("no connection string is set"))
	}

	statushooks.SetStatus(ctx, "Connecting to remote Steampipe database")
	client, err := db_client.NewDbClient(ctx, connectionString, onConnectionCallback, opts...)
	return client, error_helpers.NewErrorsAndWarning(err)
}

func (i *InitData) Cleanup(ctx context.Context) {
	if i.Client != nil {
		i.Client.Close(ctx)
	}
	if i.ShutdownTelemetry != nil {
		i.ShutdownTelemetry()
	}
	if i.Workspace != nil {
		i.Workspace.Close()
	}
}
