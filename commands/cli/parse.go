package cli

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strings"

	cmds "github.com/ipfs/go-ipfs/commands"
	files "github.com/ipfs/go-ipfs/commands/files"
	u "github.com/ipfs/go-ipfs/util"
	levenshtein "github.com/texttheater/golang-levenshtein/levenshtein"

)

// Parse parses the input commandline string (cmd, flags, and args).
// returns the corresponding command Request object.
func Parse(input []string, stdin *os.File, root *cmds.Command) (cmds.Request, *cmds.Command, []string, error) {
	path, opts, stringVals, cmd, err := parseOpts(input, root)
	if err != nil {
		return nil, nil, path, err
	}

	optDefs, err := root.GetOptions(path)
	if err != nil {
		return nil, cmd, path, err
	}

	req, err := cmds.NewRequest(path, opts, nil, nil, cmd, optDefs)
	if err != nil {
		return nil, cmd, path, err
	}

	// if -r is provided, and it is associated with the package builtin
	// recursive path option, allow recursive file paths
	recursiveOpt := req.Option(cmds.RecShort)
	recursive := false
	if recursiveOpt != nil && recursiveOpt.Definition() == cmds.OptionRecursivePath {
		recursive, _, err = recursiveOpt.Bool()
		if err != nil {
			return req, nil, nil, u.ErrCast()
		}
	}

	stringArgs, fileArgs, err := parseArgs(stringVals, stdin, cmd.Arguments, recursive, root)
	if err != nil {
		return req, cmd, path, err
	}
	req.SetArguments(stringArgs)

	file := files.NewSliceFile("", fileArgs)
	req.SetFiles(file)

	err = cmd.CheckArguments(req)
	if err != nil {
		return req, cmd, path, err
	}

	return req, cmd, path, nil
}

// Parse a command line made up of sub-commands, short arguments, long arguments and positional arguments
func parseOpts(args []string, root *cmds.Command) (
	path []string,
	opts map[string]interface{},
	stringVals []string,
	cmd *cmds.Command,
	err error,
) {
	path = make([]string, 0, len(args))
	stringVals = make([]string, 0, len(args))
	optDefs := map[string]cmds.Option{}
	opts = map[string]interface{}{}
	cmd = root

	// parseFlag checks that a flag is valid and saves it into opts
	// Returns true if the optional second argument is used
	parseFlag := func(name string, arg *string, mustUse bool) (bool, error) {
		if _, ok := opts[name]; ok {
			return false, fmt.Errorf("Duplicate values for option '%s'", name)
		}

		optDef, found := optDefs[name]
		if !found {
			err = fmt.Errorf("Unrecognized option '%s'", name)
			return false, err
		}

		if optDef.Type() == cmds.Bool {
			if mustUse {
				return false, fmt.Errorf("Option '%s' takes no arguments, but was passed '%s'", name, *arg)
			}
			opts[name] = ""
			return false, nil
		} else {
			if arg == nil {
				return true, fmt.Errorf("Missing argument for option '%s'", name)
			}
			opts[name] = *arg
			return true, nil
		}
	}

	optDefs, err = root.GetOptions(path)
	if err != nil {
		return
	}

	consumed := false
	for i, arg := range args {
		switch {
		case consumed:
			// arg was already consumed by the preceding flag
			consumed = false
			continue

		case arg == "--":
			// treat all remaining arguments as positional arguments
			stringVals = append(stringVals, args[i+1:]...)
			return

		case strings.HasPrefix(arg, "--"):
			// arg is a long flag, with an optional argument specified
			// using `=' or in args[i+1]
			var slurped bool
			var next *string
			split := strings.SplitN(arg, "=", 2)
			if len(split) == 2 {
				slurped = false
				arg = split[0]
				next = &split[1]
			} else {
				slurped = true
				if i+1 < len(args) {
					next = &args[i+1]
				} else {
					next = nil
				}
			}
			consumed, err = parseFlag(arg[2:], next, len(split) == 2)
			if err != nil {
				return
			}
			if !slurped {
				consumed = false
			}

		case strings.HasPrefix(arg, "-") && arg != "-":
			// args is one or more flags in short form, followed by an optional argument
			// all flags except the last one have type bool
			for arg = arg[1:]; len(arg) != 0; arg = arg[1:] {
				var rest *string
				var slurped bool
				mustUse := false
				if len(arg) > 1 {
					slurped = false
					str := arg[1:]
					if len(str) > 0 && str[0] == '=' {
						str = str[1:]
						mustUse = true
					}
					rest = &str
				} else {
					slurped = true
					if i+1 < len(args) {
						rest = &args[i+1]
					} else {
						rest = nil
					}
				}
				var end bool
				end, err = parseFlag(arg[0:1], rest, mustUse)
				if err != nil {
					return
				}
				if end {
					consumed = slurped
					break
				}
			}

		default:
			// arg is a sub-command or a positional argument
			sub := cmd.Subcommand(arg)
			if sub != nil {
				cmd = sub
				path = append(path, arg)
				optDefs, err = root.GetOptions(path)
				if err != nil {
					return
				}
			} else {
				stringVals = append(stringVals, arg)
			}
		}
	}
	return
}

func parseArgs(inputs []string, stdin *os.File, argDefs []cmds.Argument, recursive bool, root *cmds.Command) ([]string, []files.File, error) {
	// ignore stdin on Windows
	if runtime.GOOS == "windows" {
		stdin = nil
	}

	// check if stdin is coming from terminal or is being piped in
	if stdin != nil {
		if term, err := isTerminal(stdin); err != nil {
			return nil, nil, err
		} else if term {
			stdin = nil // set to nil so we ignore it
		}
	}

	// count required argument definitions
	numRequired := 0
	for _, argDef := range argDefs {
		if argDef.Required {
			numRequired++
		}
	}

	// count number of values provided by user.
	// if there is at least one ArgDef, we can safely trigger the inputs loop
	// below to parse stdin.
	numInputs := len(inputs)
	if len(argDefs) > 0 && argDefs[len(argDefs)-1].SupportsStdin && stdin != nil {
		numInputs += 1
	}

	searchUnknownCmd := func(args []string) (suggestions []string, err error) {
		arg := args[0]
		const MIN_LEVENSHTEIN = 3

		var options levenshtein.Options = levenshtein.Options{
				InsCost: 1,
				DelCost: 3,
				SubCost: 2,
				Matches: func(sourceCharacter rune, targetCharacter rune) bool {
					return sourceCharacter == targetCharacter
				},
		}

		// Start with a simple strings.Contains check
		for name, _ := range(root.Subcommands) {
			if strings.Contains(arg, name) {
				suggestions = append(suggestions, name)
			}
		}

		// If the string compare returns a match 
		if len(suggestions) > 0 {
			return
		}

		for name, _ := range(root.Subcommands) {
			i := levenshtein.DistanceForStrings([]rune(arg), []rune(name), options)
			// fmt.Printf("Comparing %s to %s: %d\n", arg, name, i)
			if i <= MIN_LEVENSHTEIN {
				suggestions = append(suggestions, name)
			}
		}
		// TODO Sort the suggestions based on levenshtein 
		// IDEA - Generate a suggestions map[name]int then sort by the value
		return
	}


	// if we have more arg values provided than argument definitions,
	// and the last arg definition is not variadic (or there are no definitions), return an error
	notVariadic := len(argDefs) == 0 || !argDefs[len(argDefs)-1].Variadic
	if notVariadic && len(inputs) > len(argDefs) {
		suggestions, _ := searchUnknownCmd(inputs)

		if len(suggestions) > 1 {
			return nil, nil, fmt.Errorf("Unknown Command \"%v\"\n\nDid you mean any of these?\n\n\t%s", inputs[0], strings.Join(suggestions, "\n\t"))
		} else if len(suggestions) > 0 {
			return nil, nil, fmt.Errorf("Unknown Command \"%v\"\n\nDid you mean this?\n\n\t%s", inputs[0], suggestions[0])
		} else {
			return nil, nil, fmt.Errorf("Unknown Command \"%v\"\n", inputs[0])
		}
	}

	stringArgs := make([]string, 0, numInputs)
	fileArgs := make([]files.File, 0, numInputs)

	argDefIndex := 0 // the index of the current argument definition
	for i := 0; i < numInputs; i++ {
		argDef := getArgDef(argDefIndex, argDefs)

		// skip optional argument definitions if there aren't sufficient remaining inputs
		for numInputs-i <= numRequired && !argDef.Required {
			argDefIndex++
			argDef = getArgDef(argDefIndex, argDefs)
		}
		if argDef.Required {
			numRequired--
		}

		var err error
		if argDef.Type == cmds.ArgString {
			if stdin == nil || !argDef.SupportsStdin {
				// add string values
				stringArgs, inputs = appendString(stringArgs, inputs)

			} else {
				if len(inputs) > 0 {
					// don't use stdin if we have inputs
					stdin = nil
				} else {
					// if we have a stdin, read it in and use the data as a string value
					stringArgs, stdin, err = appendStdinAsString(stringArgs, stdin)
					if err != nil {
						return nil, nil, err
					}
				}
			}
		} else if argDef.Type == cmds.ArgFile {
			if stdin == nil || !argDef.SupportsStdin {
				// treat stringArg values as file paths
				fileArgs, inputs, err = appendFile(fileArgs, inputs, argDef, recursive)
				if err != nil {
					return nil, nil, err
				}

			} else {
				if len(inputs) > 0 {
					// don't use stdin if we have inputs
					stdin = nil
				} else {
					// if we have a stdin, create a file from it
					fileArgs, stdin = appendStdinAsFile(fileArgs, stdin)
				}
			}
		}

		argDefIndex++
	}

	// check to make sure we didn't miss any required arguments
	if len(argDefs) > argDefIndex {
		for _, argDef := range argDefs[argDefIndex:] {
			if argDef.Required {
				return nil, nil, fmt.Errorf("Argument '%s' is required", argDef.Name)
			}
		}
	}

	return stringArgs, fileArgs, nil
}

func getArgDef(i int, argDefs []cmds.Argument) *cmds.Argument {
	if i < len(argDefs) {
		// get the argument definition (usually just argDefs[i])
		return &argDefs[i]

	} else if len(argDefs) > 0 {
		// but if i > len(argDefs) we use the last argument definition)
		return &argDefs[len(argDefs)-1]
	}

	// only happens if there aren't any definitions
	return nil
}

func appendString(args, inputs []string) ([]string, []string) {
	return append(args, inputs[0]), inputs[1:]
}

func appendStdinAsString(args []string, stdin *os.File) ([]string, *os.File, error) {
	buf := new(bytes.Buffer)

	_, err := buf.ReadFrom(stdin)
	if err != nil {
		return nil, nil, err
	}

	input := strings.TrimSpace(buf.String())
	return append(args, strings.Split(input, "\n")...), nil, nil
}

func appendFile(args []files.File, inputs []string, argDef *cmds.Argument, recursive bool) ([]files.File, []string, error) {
	path := inputs[0]

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}

	stat, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}

	if stat.IsDir() {
		if !argDef.Recursive {
			err = fmt.Errorf("Invalid path '%s', argument '%s' does not support directories",
				path, argDef.Name)
			return nil, nil, err
		}
		if !recursive {
			err = fmt.Errorf("'%s' is a directory, use the '-%s' flag to specify directories",
				path, cmds.RecShort)
			return nil, nil, err
		}
	}

	arg, err := files.NewSerialFile(path, file)
	if err != nil {
		return nil, nil, err
	}

	return append(args, arg), inputs[1:], nil
}

func appendStdinAsFile(args []files.File, stdin *os.File) ([]files.File, *os.File) {
	arg := files.NewReaderFile("", stdin, nil)
	return append(args, arg), nil
}

// isTerminal returns true if stdin is a Stdin pipe (e.g. `cat file | ipfs`),
// and false otherwise (e.g. nothing is being piped in, so stdin is
// coming from the terminal)
func isTerminal(stdin *os.File) (bool, error) {
	stat, err := stdin.Stat()
	if err != nil {
		return false, err
	}

	// if stdin is a CharDevice, return true
	return ((stat.Mode() & os.ModeCharDevice) != 0), nil
}
