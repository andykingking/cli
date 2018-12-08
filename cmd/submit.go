package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"github.com/exercism/cli/api"
	"github.com/exercism/cli/config"
	"github.com/exercism/cli/workspace"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// submitCmd lets people upload a solution to the website.
var submitCmd = &cobra.Command{
	Use:     "submit FILE1 [FILE2 ...]",
	Aliases: []string{"s"},
	Short:   "Submit your solution to an exercise.",
	Long: `Submit your solution to an Exercism exercise.

	Call the command with the list of files you want to submit.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewConfig()

		usrCfg := viper.New()
		usrCfg.AddConfigPath(cfg.Dir)
		usrCfg.SetConfigName("user")
		usrCfg.SetConfigType("json")
		// Ignore error. If the file doesn't exist, that is fine.
		_ = usrCfg.ReadInConfig()
		cfg.UserViperConfig = usrCfg

		v := viper.New()
		v.AddConfigPath(cfg.Dir)
		v.SetConfigName("cli")
		v.SetConfigType("json")
		// Ignore error. If the file doesn't exist, that is fine.
		_ = v.ReadInConfig()

		return runSubmit(cfg, cmd.Flags(), args)
	},
}

type submitContext struct {
	args   []string
	usrCfg *viper.Viper
}

func runSubmit(cfg config.Config, flags *pflag.FlagSet, args []string) error {
	usrCfg := cfg.UserViperConfig

	if err := validateUserConfig(usrCfg); err != nil {
		return err
	}

	ctx := &submitContext{args: args, usrCfg: usrCfg}

	if err := ctx.sanitizeArgs(); err != nil {
		return err
	}

	exerciseDir, err := ctx.exerciseDir()
	if err != nil {
		return err
	}

	exercise := workspace.NewExerciseFromDir(exerciseDir)

	migrationStatus, err := exercise.MigrateLegacyMetadataFile()
	if err != nil {
		return err
	}
	if verbose, _ := flags.GetBool("verbose"); verbose {
		fmt.Fprintf(Err, migrationStatus.String())
	}

	metadata, err := ctx.metadata(exerciseDir)
	if err != nil {
		return err
	}

	exercise.Documents, err = ctx.documents(exerciseDir)
	if err != nil {
		return err
	}

	if err := ctx.submitRequest(metadata.ID, exercise.Documents); err != nil {
		return err
	}

	msg := `

	Your solution has been submitted successfully.
	%s
`
	suffix := "View it at:\n\n    "
	if metadata.AutoApprove && metadata.Team == "" {
		suffix = "You can complete the exercise and unlock the next core exercise at:\n"
	}
	fmt.Fprintf(Err, msg, suffix)
	fmt.Fprintf(Out, "    %s\n\n", metadata.URL)
	return nil
}

func (ctx *submitContext) sanitizeArgs() error {
	for i, arg := range ctx.args {
		var err error
		arg, err = filepath.Abs(arg)
		if err != nil {
			return err
		}

		info, err := os.Lstat(arg)
		if err != nil {
			if os.IsNotExist(err) {
				msg := `

	The file you are trying to submit cannot be found.

		%s

		`
				return fmt.Errorf(msg, arg)
			}
			return err
		}
		if info.IsDir() {
			msg := `

	You are submitting a directory, which is not currently supported.

		%s

	Please change into the directory and provide the path to the file(s) you wish to submit

		%s submit FILENAME

			`
			return fmt.Errorf(msg, arg, BinaryName)
		}

		src, err := filepath.EvalSymlinks(arg)
		if err != nil {
			return err
		}
		ctx.args[i] = src
	}
	return nil
}

func (ctx *submitContext) exerciseDir() (string, error) {
	ws, err := workspace.New(ctx.usrCfg.GetString("workspace"))
	if err != nil {
		return "", err
	}

	var exerciseDir string
	for _, arg := range ctx.args {
		dir, err := ws.ExerciseDir(arg)
		if err != nil {
			if workspace.IsMissingMetadata(err) {
				return "", errors.New(msgMissingMetadata)
			}
			return "", err
		}
		if exerciseDir != "" && dir != exerciseDir {
			msg := `

	You are submitting files belonging to different solutions.
	Please submit the files for one solution at a time.

		`
			return "", errors.New(msg)
		}
		exerciseDir = dir
	}
	return exerciseDir, nil
}

func (ctx *submitContext) metadata(exerciseDir string) (*workspace.ExerciseMetadata, error) {
	metadata, err := workspace.NewExerciseMetadata(exerciseDir)
	if err != nil {
		return nil, err
	}

	exercise := workspace.NewExerciseFromDir(exerciseDir)
	if exercise.Slug != metadata.Exercise {
		// TODO: error msg should suggest running future doctor command
		msg := `

	The exercise directory does not match exercise slug in metadata:

		expected '%[1]s' but got '%[2]s'

	Please rename the directory '%[1]s' to '%[2]s' and try again.

		`
		return nil, fmt.Errorf(msg, exercise.Slug, metadata.Exercise)
	}

	if !metadata.IsRequester {
		// TODO: add test
		msg := `

	The solution you are submitting is not connected to your account.
	Please re-download the exercise to make sure it has the data it needs.

		%s download --exercise=%s --track=%s

		`
		return nil, fmt.Errorf(msg, BinaryName, metadata.Exercise, metadata.Track)
	}
	return metadata, nil
}

func (ctx *submitContext) documents(exerciseDir string) ([]workspace.Document, error) {
	docs := make([]workspace.Document, 0, len(ctx.args))
	for _, file := range ctx.args {
		// Don't submit empty files
		info, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		const maxFileSize int64 = 65535
		if info.Size() >= maxFileSize {
			msg := `

      The submitted file '%s' is larger than the max allowed file size of %d bytes.
      Please reduce the size of the file and try again.

			`
			return nil, fmt.Errorf(msg, file, maxFileSize)
		}
		if info.Size() == 0 {

			msg := `

    WARNING: Skipping empty file
             %s

		`
			fmt.Fprintf(Err, msg, file)
			continue
		}
		doc, err := workspace.NewDocument(exerciseDir, file)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		msg := `

	No files found to submit.

		`
		return nil, errors.New(msg)
	}
	return docs, nil
}

func (ctx *submitContext) submitRequest(id string, docs []workspace.Document) error {
	if len(docs) == 0 {
		return errors.New("docs is empty")
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for _, doc := range docs {
		file, err := os.Open(doc.Filepath())
		if err != nil {
			return err
		}
		defer file.Close()

		part, err := writer.CreateFormFile("files[]", doc.Path())
		if err != nil {
			return err
		}
		_, err = io.Copy(part, file)
		if err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}

	client, err := api.NewClient(ctx.usrCfg.GetString("token"), ctx.usrCfg.GetString("apibaseurl"))
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/solutions/%s", ctx.usrCfg.GetString("apibaseurl"), id)
	req, err := client.NewRequest("PATCH", url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest {
		var jsonErrBody apiErrorMessage
		if err := json.NewDecoder(resp.Body).Decode(&jsonErrBody); err != nil {
			return fmt.Errorf("failed to parse error response - %s", err)
		}

		return fmt.Errorf(jsonErrBody.Error.Message)
	}

	bb := &bytes.Buffer{}
	_, err = bb.ReadFrom(resp.Body)
	if err != nil {
		return err
	}
	return nil
}

func init() {
	RootCmd.AddCommand(submitCmd)
}

type apiErrorMessage struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
