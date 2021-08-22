package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/faunists/deal-go/entities"
	"github.com/faunists/deal-go/processors"
)

const (
	contextPackage = protogen.GoImportPath("context")
	testingPackage = protogen.GoImportPath("testing")
	logPackage     = protogen.GoImportPath("log")
	netPackage     = protogen.GoImportPath("net")
	grpcPackage    = protogen.GoImportPath("google.golang.org/grpc")
	grpcCodes      = protogen.GoImportPath("google.golang.org/grpc/codes")
	grpcStatus     = protogen.GoImportPath("google.golang.org/grpc/status")
	protoPackage   = protogen.GoImportPath("google.golang.org/protobuf/proto")
	buffconPackage = protogen.GoImportPath("google.golang.org/grpc/test/bufconn")
)

var (
	contextContext = contextPackage.Ident("Context")
	testingT       = testingPackage.Ident("T")
)

func main() { //nolint:gocognit // this function set flags and verify them, after generate the code
	var flags flag.FlagSet

	contractFilePath := flags.String("contract-file", "", "Path to your contract file")

	protogen.Options{
		ParamFunc: flags.Set,
	}.Run(func(plugin *protogen.Plugin) error {
		plugin.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)

		if *contractFilePath == "" {
			return fmt.Errorf("'contract-file' option not provided")
		}

		for _, file := range plugin.Files {
			if file.Generate {
				_, err := generateContracts(plugin, file, *contractFilePath)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
}

func generateContracts(
	plugin *protogen.Plugin,
	file *protogen.File,
	contractFilePath string,
) (*protogen.GeneratedFile, error) {
	if len(file.Services) == 0 {
		return nil, nil
	}

	// Parse contract JSON file that was defined in the service options
	rawContract, err := processors.ReadContractFile(contractFilePath)
	if err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("%s_contract.pb.go", file.GeneratedFilenamePrefix)
	newFile := plugin.NewGeneratedFile(filename, file.GoImportPath)

	writeHeader(file, newFile)

	for _, service := range file.Services {

		// Verifies if the file has a contract for the given service
		serviceContract, hasContract := rawContract.Services[service.GoName]
		if !hasContract {
			continue
		}

		err = generateClient(newFile, service, serviceContract)
		if err != nil {
			return nil, err
		}

		err = generateServerTest(newFile, service, serviceContract)
		if err != nil {
			return nil, err
		}
	}

	return newFile, nil
}

func writeHeader(originalFile *protogen.File, generatedFile *protogen.GeneratedFile) {
	generatedFile.P("// Code generated by protoc-gen-go-deal. DO NOT EDIT.")
	generatedFile.P("//")
	generatedFile.P("// versions:")
	generatedFile.P("//   - protoc")
	generatedFile.P()
	generatedFile.P(fmt.Sprintf("package %s", originalFile.GoPackageName))
	generatedFile.P()
}

func generateClient(
	file *protogen.GeneratedFile,
	service *protogen.Service,
	contractService entities.Service,
) error {
	clientName := fmt.Sprintf("%sContractClient", processors.MakeExportedName(service.GoName))

	// Create client struct
	file.P(fmt.Sprintf("type %s struct {}", clientName))

	// Iterate over the service methods and generate the proper method containing a
	// switch case based on the Request/Response provided by the user through JSON File
	for _, method := range service.Methods {
		// We don't need to care about if a contract to the specific method exists or not,
		// 'cause the method will be created with a default switch case in order to satisfy
		// the client interface generated by `protoc-gen-go-grpc`.
		methodContract := contractService[method.GoName]
		switchCase, err := generateClientCases(file, method, methodContract)
		if err != nil {
			return err
		}

		file.P(
			fmt.Sprintf(
				"func (_ %s) %s(ctx %s, in *%s, opts ...%s) (*%s, error) {%s}",
				clientName,
				method.GoName,
				file.QualifiedGoIdent(contextContext),
				file.QualifiedGoIdent(method.Input.GoIdent),
				file.QualifiedGoIdent(grpcPackage.Ident("CallOption")),
				file.QualifiedGoIdent(method.Output.GoIdent),
				switchCase,
			),
		)
		file.P()
	}
	return nil
}

func generateClientCases(
	file *protogen.GeneratedFile,
	method *protogen.Method,
	methodContract entities.Method,
) (string, error) {
	switchCase := bytes.NewBufferString("switch {")

	err := generateSuccessCases(file, method, methodContract.SuccessCases, switchCase)
	if err != nil {
		return "", fmt.Errorf("failed to generate the success cases: %w", err)
	}

	err = generateFailureCases(file, method, methodContract.FailureCases, switchCase)
	if err != nil {
		return "", fmt.Errorf("failed to generate the failure cases: %w", err)
	}

	// Default case if no cases are provided
	switchCase.WriteString("default: return nil, nil }")

	return switchCase.String(), nil
}

func generateSuccessCases(
	file *protogen.GeneratedFile,
	method *protogen.Method,
	cases []entities.SuccessCase,
	writer io.StringWriter,
) error {
	for _, successCase := range cases {
		requestRepresentation, err := getProtoRepresentation(
			successCase.Request, method.Input, file,
		)
		if err != nil {
			return err
		}

		responseRepresentation, err := getProtoRepresentation(
			successCase.Response, method.Output, file,
		)
		if err != nil {
			return err
		}

		_, err = writer.WriteString(
			fmt.Sprintf(
				"case %s(in, %s):\n// Description: %s\n return %s, nil\n",
				file.QualifiedGoIdent(protoPackage.Ident("Equal")),
				requestRepresentation,
				successCase.Description,
				responseRepresentation,
			),
		)
		if err != nil {
			return fmt.Errorf("error writing a success case: %w", err)
		}
	}

	return nil
}

func generateFailureCases(
	file *protogen.GeneratedFile,
	method *protogen.Method,
	cases []entities.FailureCase,
	writer io.StringWriter,
) error {
	for _, failureCase := range cases {
		requestRepresentation, err := getProtoRepresentation(
			failureCase.Request, method.Input, file,
		)
		if err != nil {
			return err
		}

		if !processors.IsErrorCodeValid(failureCase.Error.ErrorCode) {
			return fmt.Errorf("invalid error code: %s", failureCase.Error.ErrorCode)
		}

		_, err = writer.WriteString(
			fmt.Sprintf(
				"case %s(in, %s):\n// Description: %s\n return nil, %s(%s, %q)\n",
				file.QualifiedGoIdent(protoPackage.Ident("Equal")),
				requestRepresentation,
				failureCase.Description,
				file.QualifiedGoIdent(grpcStatus.Ident("Errorf")),
				file.QualifiedGoIdent(grpcCodes.Ident(failureCase.Error.ErrorCode)),
				failureCase.Error.Message,
			),
		)
		if err != nil {
			return fmt.Errorf("error writing a success case: %w", err)
		}
	}

	return nil
}

func getProtoRepresentation(
	r interface{},
	message *protogen.Message,
	file *protogen.GeneratedFile,
) (string, error) {
	marshaledRequest, err := json.Marshal(r)
	if err != nil {
		return "", err
	}

	messageArguments, err := inputOutputToString(marshaledRequest, message)
	if err != nil {
		return "", fmt.Errorf("failed to generate message representation: %w", err)
	}

	return fmt.Sprintf(
		"&%s{%s}",
		file.QualifiedGoIdent(message.GoIdent),
		strings.Join(messageArguments, ","),
	), nil
}

func inputOutputToString(data []byte, message *protogen.Message) ([]string, error) {
	// This step validates the data provided by the user through JSON file
	methodInputMessage := dynamicpb.NewMessage(message.Desc)
	err := protojson.Unmarshal(data, methodInputMessage)
	if err != nil {
		return nil, err
	}

	// Making this map we're able to correlate a field with a field descriptor
	fieldsMapByNumber := make(map[protoreflect.FieldNumber]*protogen.Field)
	for _, field := range message.Fields {
		fieldsMapByNumber[field.Desc.Number()] = field
	}

	// Try to get all of the populated fields (name and value)
	messageArguments := make([]string, 0)
	methodInputMessage.Range(
		func(descriptor protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			field, exists := fieldsMapByNumber[descriptor.Number()]
			if !exists {
				err = fmt.Errorf(
					"field not found %s while inspecting message %s",
					descriptor.Name(), message.Desc.Name(),
				)
				return false
			}

			messageArguments = append(
				messageArguments,
				fmt.Sprintf("%s: %s", field.GoName, processors.FormatFieldValue(value)),
			)

			return true
		},
	)
	if err != nil {
		return nil, err
	}

	return messageArguments, nil
}

func generateServerTest(
	file *protogen.GeneratedFile,
	service *protogen.Service,
	contractService entities.Service,
) error {
	functionName := fmt.Sprintf("%sContractTest", processors.MakeExportedName(service.GoName))
	file.P(
		fmt.Sprintf(
			"func %s(t *%s, ctx %s, server *%s) {",
			functionName,
			file.QualifiedGoIdent(testingT),
			file.QualifiedGoIdent(contextContext),
			file.QualifiedGoIdent(grpcPackage.Ident("Server")),
		),
	)

	file.P("// gRPC Server setup")
	file.P("bufSize := 1024 * 1024")
	file.P(
		fmt.Sprintf(
			"bufferListener := %s(bufSize)",
			file.QualifiedGoIdent(buffconPackage.Ident("Listen")),
		),
	)

	file.P("go func() {")
	file.P(
		fmt.Sprintf(`if err := server.Serve(bufferListener); err != nil {
				%s("Contract Server test exited with error: %%v", err)
			}`,
			file.QualifiedGoIdent(logPackage.Ident("Fatalf")),
		),
	)
	file.P("}()")
	file.P("defer server.Stop()")
	file.P()

	file.P("// gRPC Client setup")
	file.P(
		fmt.Sprintf(
			"dialer := func(_ %s, _ string) (%s, error) { return bufferListener.Dial() }",
			file.QualifiedGoIdent(contextContext),
			file.QualifiedGoIdent(netPackage.Ident("Conn")),
		),
	)
	file.P(
		fmt.Sprintf(
			`clientConn, err := %s(ctx, "bufnet", %s(dialer), %s())`,
			file.QualifiedGoIdent(grpcPackage.Ident("DialContext")),
			file.QualifiedGoIdent(grpcPackage.Ident("WithContextDialer")),
			file.QualifiedGoIdent(grpcPackage.Ident("WithInsecure")),
		),
	)
	file.P(`if err != nil { t.Fatalf("Failed to dial bufnet: %v", err) }`)
	file.P("defer clientConn.Close()")
	file.P()

	// We're creating a client this way believing on what go-grpc will generate
	// and both will be in the same package.
	file.P(fmt.Sprintf("client := New%sClient(clientConn)", service.GoName))
	file.P(fmt.Sprintf("run%sTests(t, ctx, client)", service.GoName))

	file.P("}\n")

	return generateSuccessAndFailureTests(file, service, contractService)
}

func generateSuccessAndFailureTests(
	file *protogen.GeneratedFile,
	service *protogen.Service,
	contractService entities.Service,
) error {
	file.P(
		fmt.Sprintf(
			"func run%sTests(t *%s, ctx %s, client %sClient) {",
			service.GoName,
			file.QualifiedGoIdent(testingT),
			file.QualifiedGoIdent(contextContext),
			service.GoName,
		),
	)

	for _, method := range service.Methods {
		methodContract, exists := contractService[method.GoName]
		if !exists {
			continue
		}

		file.P(
			fmt.Sprintf(
				`t.Run("Contract test for '%s' method", func(t *%s) {`,
				method.GoName,
				file.QualifiedGoIdent(testingT),
			),
		)

		err := generateSuccessTestForServer(file, method, methodContract.SuccessCases)
		if err != nil {
			return err
		}

		err = generateFailureTestForServer(file, method, methodContract.FailureCases)
		if err != nil {
			return err
		}

		file.P("})")
	}

	file.P("}")

	return nil
}

func generateSuccessTestForServer(
	file *protogen.GeneratedFile,
	method *protogen.Method,
	successCases []entities.SuccessCase,
) error {
	file.P(
		fmt.Sprintf(
			`t.Run("Success Cases", func(t *%s) {`,
			file.QualifiedGoIdent(testingT),
		),
	)
	file.P(
		fmt.Sprintf(
			"tests := []struct {name string\nrequest *%s\nexpectedResponse *%s} {",
			file.QualifiedGoIdent(method.Input.GoIdent),
			file.QualifiedGoIdent(method.Output.GoIdent),
		),
	)

	for _, successCase := range successCases {
		requestRepresentation, err := getProtoRepresentation(
			successCase.Request, method.Input, file,
		)
		if err != nil {
			return err
		}

		responseRepresentation, err := getProtoRepresentation(
			successCase.Response, method.Output, file,
		)
		if err != nil {
			return err
		}

		file.P(
			fmt.Sprintf(
				"{\nname: \"%s\",\nrequest: %s,\nexpectedResponse: %s,\n},",
				successCase.Description,
				requestRepresentation,
				responseRepresentation,
			),
		)
	}
	file.P("}")

	file.P()
	file.P(
		fmt.Sprintf(`for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					response, err := client.%s(ctx, test.request)
					if err != nil {
						t.Fatalf("unexpected error happened: %%w", err)
					}

					if !proto.Equal(response, test.expectedResponse) {
						t.Fatalf(
							"expected response: %%v, given response: %%v",
							test.expectedResponse, response,
						)
					}
				})
			}`,
			method.GoName,
		),
	)
	file.P("})")

	return nil
}

func generateFailureTestForServer(
	file *protogen.GeneratedFile,
	method *protogen.Method,
	failureCases []entities.FailureCase,
) error {
	file.P()
	file.P(
		fmt.Sprintf(
			`t.Run("Failure Cases", func(t *%s) {`,
			file.QualifiedGoIdent(testingT),
		),
	)
	file.P(
		fmt.Sprintf(
			"tests := []struct {name string\nrequest *%s\nexpectedError string} {",
			file.QualifiedGoIdent(method.Input.GoIdent),
		),
	)

	for _, failureCase := range failureCases {
		requestRepresentation, err := getProtoRepresentation(
			failureCase.Request, method.Input, file,
		)
		if err != nil {
			return err
		}

		file.P(
			fmt.Sprintf(
				"{\nname: \"%s\",\nrequest: %s,\nexpectedError: \"%s\",\n},",
				failureCase.Description,
				requestRepresentation,
				failureCase.Error,
			),
		)
	}
	file.P("}")

	file.P()
	file.P(
		fmt.Sprintf(`for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					_, err := client.%s(ctx, test.request)
					if err == nil {
						t.Fatalf("an error was expected but no one was returned")
					}

					if err.Error() != test.expectedError {
						t.Fatalf("expected error: %%s, given error: %%s", test.expectedError, err)
					}
				})
			}`,
			method.GoName,
		),
	)
	file.P("})")

	return nil
}
