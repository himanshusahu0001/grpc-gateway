package gengateway

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/golang/glog"
	"github.com/grpc-ecosystem/grpc-gateway/v2/internal/casing"
	"github.com/grpc-ecosystem/grpc-gateway/v2/internal/descriptor"
	"github.com/grpc-ecosystem/grpc-gateway/v2/utilities"
)

type param struct {
	*descriptor.File
	Imports            []descriptor.GoPackage
	UseRequestContext  bool
	RegisterFuncSuffix string
	AllowPatchFeature  bool
	OmitPackageDoc     bool
}

type binding struct {
	*descriptor.Binding
	Registry          *descriptor.Registry
	AllowPatchFeature bool
}

// GetBodyFieldPath returns the binding body's field path.
func (b binding) GetBodyFieldPath() string {
	if b.Body != nil && len(b.Body.FieldPath) != 0 {
		return b.Body.FieldPath.String()
	}
	return "*"
}

func GetCamelcase(in string) string {
	return casing.Camel(in)
}

// GetBodyFieldStructName returns the binding body's struct field name.
func (b binding) GetBodyFieldStructName() (string, error) {
	if b.Body != nil && len(b.Body.FieldPath) != 0 {
		return casing.Camel(b.Body.FieldPath.String()), nil
	}
	return "", errors.New("no body field found")
}

// HasQueryParam determines if the binding needs parameters in query string.
//
// It sometimes returns true even though actually the binding does not need.
// But it is not serious because it just results in a small amount of extra codes generated.
func (b binding) HasQueryParam() bool {
	if b.Body != nil && len(b.Body.FieldPath) == 0 {
		return false
	}
	fields := make(map[string]bool)
	for _, f := range b.Method.RequestType.Fields {
		fields[f.GetName()] = true
	}
	if b.Body != nil {
		delete(fields, b.Body.FieldPath.String())
	}
	for _, p := range b.PathParams {
		delete(fields, p.FieldPath.String())
	}
	return len(fields) > 0
}

func (b binding) QueryParamFilter() queryParamFilter {
	var seqs [][]string
	if b.Body != nil {
		seqs = append(seqs, strings.Split(b.Body.FieldPath.String(), "."))
	}
	for _, p := range b.PathParams {
		seqs = append(seqs, strings.Split(p.FieldPath.String(), "."))
	}
	return queryParamFilter{utilities.NewDoubleArray(seqs)}
}

// HasEnumPathParam returns true if the path parameter slice contains a parameter
// that maps to an enum proto field that is not repeated, if not false is returned.
func (b binding) HasEnumPathParam() bool {
	return b.hasEnumPathParam(false)
}

// HasRepeatedEnumPathParam returns true if the path parameter slice contains a parameter
// that maps to a repeated enum proto field, if not false is returned.
func (b binding) HasRepeatedEnumPathParam() bool {
	return b.hasEnumPathParam(true)
}

// hasEnumPathParam returns true if the path parameter slice contains a parameter
// that maps to a enum proto field and that the enum proto field is or isn't repeated
// based on the provided 'repeated' parameter.
func (b binding) hasEnumPathParam(repeated bool) bool {
	for _, p := range b.PathParams {
		if p.IsEnum() && p.IsRepeated() == repeated {
			return true
		}
	}
	return false
}

// LookupEnum looks up a enum type by path parameter.
func (b binding) LookupEnum(p descriptor.Parameter) *descriptor.Enum {
	e, err := b.Registry.LookupEnum("", p.Target.GetTypeName())
	if err != nil {
		return nil
	}
	return e
}

// FieldMaskField returns the golang-style name of the variable for a FieldMask, if there is exactly one of that type in
// the message. Otherwise, it returns an empty string.
func (b binding) FieldMaskField() string {
	var fieldMaskField *descriptor.Field
	for _, f := range b.Method.RequestType.Fields {
		if f.GetTypeName() == ".google.protobuf.FieldMask" {
			// if there is more than 1 FieldMask for this request, then return none
			if fieldMaskField != nil {
				return ""
			}
			fieldMaskField = f
		}
	}
	if fieldMaskField != nil {
		return casing.Camel(fieldMaskField.GetName())
	}
	return ""
}

// queryParamFilter is a wrapper of utilities.DoubleArray which provides String() to output DoubleArray.Encoding in a stable and predictable format.
type queryParamFilter struct {
	*utilities.DoubleArray
}

func (f queryParamFilter) String() string {
	encodings := make([]string, len(f.Encoding))
	for str, enc := range f.Encoding {
		encodings[enc] = fmt.Sprintf("%q: %d", str, enc)
	}
	e := strings.Join(encodings, ", ")
	return fmt.Sprintf("&utilities.DoubleArray{Encoding: map[string]int{%s}, Base: %#v, Check: %#v}", e, f.Base, f.Check)
}

type trailerParams struct {
	P                  param
	Services           []*descriptor.Service
	UseRequestContext  bool
	RegisterFuncSuffix string
}

func applyTemplate(p param, reg *descriptor.Registry) (string, error) {
	w := bytes.NewBuffer(nil)
	if err := headerTemplate.Execute(w, p); err != nil {
		return "", err
	}
	var targetServices []*descriptor.Service

	for _, msg := range p.Messages {
		msgName := casing.Camel(*msg.Name)
		msg.Name = &msgName
	}

	for _, svc := range p.Services {
		var methodWithBindingsSeen bool
		svcName := casing.Camel(*svc.Name)
		svc.Name = &svcName

		for _, meth := range svc.Methods {
			glog.V(2).Infof("Processing %s.%s", svc.GetName(), meth.GetName())
			methName := casing.Camel(*meth.Name)
			meth.Name = &methName
			for _, b := range meth.Bindings {
				if err := reg.CheckDuplicateAnnotation(b.HTTPMethod, b.PathTmpl.Template); err != nil {
					return "", err
				}

				methodWithBindingsSeen = true
			}
		}
		if methodWithBindingsSeen {
			targetServices = append(targetServices, svc)
		}
	}
	if len(targetServices) == 0 {
		return "", errNoTargetService
	}

	tp := trailerParams{
		P:                  p,
		Services:           targetServices,
		UseRequestContext:  p.UseRequestContext,
		RegisterFuncSuffix: p.RegisterFuncSuffix,
	}
	// SDK Client
	if err := sdkClient.Execute(w, tp); err != nil {
		return "", err
	}

	return w.String(), nil
}

var (
	headerTemplate = template.Must(template.New("header").Parse(`
// Code generated by protoc-gen-coredge-sdk. DO NOT EDIT.
// source: {{.GetName}}

{{if not .OmitPackageDoc}}/*
Package {{.GoPkg.Name}} is auto generated SDK module

It provides auto generated functions to perform operations
using APIs defined as part of protobuf
*/{{end}}
package {{.GoPkg.Name}}
import (
	_ "bytes"

	{{range $i := .Imports}}{{if $i.Standard}}{{$i | printf "%s\n"}}{{end}}{{end}}

	{{range $i := .Imports}}{{if not $i.Standard}}{{$i | printf "%s\n"}}{{end}}{{end}}
)

`))

	sdkClient = template.Must(template.New("sdk-client").Funcs(
		template.FuncMap{
			"GetCamelcase": GetCamelcase,
		},
	).Parse(`
{{range $svc := .Services}}
type {{$svc.GetName}}SdkClient interface {
	{{- range $m := $svc.Methods}}
	{{$m.Name}}(req *{{$m.RequestType.GoType $m.Service.File.GoPkg.Path}}) (*{{$m.ResponseType.GoType $m.Service.File.GoPkg.Path}}, error)
	{{- end}}
}

type impl{{$svc.GetName}}Client struct {
	client gosdkclient.SdkClient
}

func New{{$svc.GetName}}SdkClient(client gosdkclient.SdkClient) {{$svc.GetName}}SdkClient {
	return &impl{{$svc.GetName}}Client{
		client: client,
	}
}

{{- range $m := $svc.Methods}}
func (c *impl{{$svc.GetName}}Client) {{$m.Name}}(req *{{$m.RequestType.GoType $m.Service.File.GoPkg.Path}}) (*{{$m.ResponseType.GoType $m.Service.File.GoPkg.Path}}, error) {
	{{- $b := (index $m.Bindings 0) }}
	// TODO(prabhjot) we are ignoring the error here for the time being
	subUrl := "{{ $b.PathTmpl.Template }}"
	{{- range $p := $b.PathParams }}
	subUrl = strings.Replace(subUrl, "{"+"{{$p.Target.Name}}"+"}", req.{{GetCamelcase $p.Target.Name}}, -1)
	{{- end }}
	marshaller := &runtime.JSONPb{}
	{{- if $b.Body }}
	jsonData, _ := marshaller.Marshal(req)
	r, _ := http.NewRequest({{$b.HTTPMethod | printf "%q"}}, subUrl, bytes.NewBuffer(jsonData))
	{{- else }}
	r, _ := http.NewRequest({{$b.HTTPMethod | printf "%q"}}, subUrl, nil)
	{{- end }}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.client.PerformReq(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	obj := &{{$m.ResponseType.GoType $m.Service.File.GoPkg.Path}}{}
	err = marshaller.Unmarshal(bodyBytes, obj)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

{{- end}}

{{end}}`))

)
