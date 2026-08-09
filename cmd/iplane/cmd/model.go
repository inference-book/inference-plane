package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// The `iplane model` verb group manages the warm-cache pin registry:
// pre-staging models onto provider volumes so later deploys mount weights
// instead of re-downloading them.
//
// These run in-process against the state file (no --service-url yet).
// Because `iplane serve` holds the state-dir lock for its lifetime,
// pinning is a pre-stage step: pin models, then start the daemon (or pin
// while no daemon is running). The lock-held error says as much.

var (
	pinProvider string
	pinRegion   string
	pinSizeGB   int
	pinHFToken  string
	pinForce    bool

	lsProvider string

	unpinModel string
)

var modelCmd = &cobra.Command{
	Use:   "model",
	Short: "Manage the warm-cache model pin registry",
	Long: "Pre-stage models onto provider volumes so deployments mount " +
		"pre-downloaded weights instead of fetching from HuggingFace on " +
		"every cold start.",
}

var modelPinCmd = &cobra.Command{
	Use:   "pin <model>",
	Short: "Stage a model onto a provider's per-region cache volume",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		model := args[0]
		if pinRegion == "" {
			return errors.New("--region is required (a volume is datacenter-locked)")
		}
		if err := ensureProviderAPIKey(pinProvider); err != nil {
			return err
		}
		svc, err := buildPinService()
		if err != nil {
			return err
		}
		hfToken := pinHFToken
		if hfToken == "" {
			hfToken = os.Getenv("HF_TOKEN")
		}
		res, err := svc.PinModel(cmd.Context(), provisioners.PinModelRequest{
			Model:    model,
			Provider: pinProvider,
			Region:   pinRegion,
			HFToken:  hfToken,
			SizeGB:   pinSizeGB,
			Force:    pinForce,
		})
		if err != nil {
			return err
		}
		v := res.Volume
		verb := "staged"
		if res.AlreadyStaged {
			verb = "already staged"
		}
		fmt.Printf("%s %s onto volume %s (%s / %s)\n", verb, model, v.GetId(), v.GetProvider(), v.GetRegion())
		fmt.Printf("  models on this volume: %s\n", strings.Join(v.GetModels(), ", "))
		fmt.Printf("  point a deployment at it with:\n")
		fmt.Printf("    model_cache: { provider: %s, volume_id: %s, mount_path: %s }\n", v.GetProvider(), v.GetId(), v.GetMountPath())
		return nil
	},
}

var modelLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List pinned cache volumes and the models staged on them",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := buildPinService()
		if err != nil {
			return err
		}
		vols, err := svc.ListVolumes(cmd.Context(), lsProvider)
		if err != nil {
			return err
		}
		if len(vols) == 0 {
			fmt.Println("no pinned volumes")
			return nil
		}
		for _, v := range vols {
			fmt.Printf("%s\t%s/%s\t%dGB\t%s\n", v.GetId(), v.GetProvider(), v.GetRegion(), v.GetSizeGb(), strings.Join(v.GetModels(), ","))
		}
		return nil
	},
}

var modelUnpinCmd = &cobra.Command{
	Use:   "unpin <volume-id>",
	Short: "Remove a model from a volume, or destroy the whole volume",
	Long: "With --model, drops that model from the volume's registry entry " +
		"(the staged files stay on the volume). Without --model, destroys " +
		"the whole volume and everything on it.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := buildPinService()
		if err != nil {
			return err
		}
		v, err := svc.UnpinModel(cmd.Context(), provisioners.UnpinRequest{
			VolumeID: args[0],
			Model:    unpinModel,
		})
		if err != nil {
			return err
		}
		if v == nil {
			fmt.Printf("destroyed volume %s\n", args[0])
			return nil
		}
		fmt.Printf("unpinned %s from %s; remaining: %s\n", unpinModel, args[0], strings.Join(v.GetModels(), ", "))
		return nil
	},
}

// buildPinService opens the state store in-process and builds a Service
// for the pin verbs. Mirrors buildDeploymentClient's in-process branch;
// there is no --service-url path yet, so a running daemon's lock is a
// hard stop (pin before serve).
func buildPinService() (*provisioners.Service, error) {
	dir, err := resolveDeploymentStateDir()
	if err != nil {
		return nil, err
	}
	store, err := file.Open(dir, deploymentOperatorID)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	if _, err := store.LockForLifetime(); err != nil {
		var held *file.ErrLockHeld
		if errors.As(err, &held) {
			if held.HolderPID != 0 {
				return nil, fmt.Errorf("iplane serve is running at PID %d (state %s); stop it and pin before serving", held.HolderPID, held.Path)
			}
			return nil, fmt.Errorf("state directory %q is locked; stop the holder and pin before serving", held.Path)
		}
		return nil, fmt.Errorf("acquire state lock: %w", err)
	}
	return buildLocalService(store, deploymentOperatorID)
}

func init() {
	rootCmd.AddCommand(modelCmd)
	modelCmd.AddCommand(modelPinCmd, modelLsCmd, modelUnpinCmd)

	modelPinCmd.Flags().StringVar(&pinProvider, "provider", defaultProvider(provisioners.ProviderRunPod), "provider to stage the volume on")
	modelPinCmd.Flags().StringVar(&pinRegion, "region", "", "datacenter/region for the volume (required)")
	modelPinCmd.Flags().IntVar(&pinSizeGB, "size", 0, "volume size in GB when creating it (default 200)")
	modelPinCmd.Flags().StringVar(&pinHFToken, "hf-token", "", "HuggingFace token for gated models (defaults to $HF_TOKEN)")
	modelPinCmd.Flags().BoolVar(&pinForce, "force", false, "re-stage even if the model is already recorded on the volume")

	modelLsCmd.Flags().StringVar(&lsProvider, "provider", "", "filter to one provider (default: all)")

	modelUnpinCmd.Flags().StringVar(&unpinModel, "model", "", "remove just this model (default: destroy the whole volume)")
}
