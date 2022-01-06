// Copyright 2020-2021 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by protoc-gen-go-apiclienttwirp. DO NOT EDIT.

package registryv1alpha1apiclienttwirp

import (
	context "context"
	v1 "github.com/bufbuild/buf/private/gen/proto/go/buf/alpha/image/v1"
	v1alpha1 "github.com/bufbuild/buf/private/gen/proto/go/buf/alpha/registry/v1alpha1"
	zap "go.uber.org/zap"
	pluginpb "google.golang.org/protobuf/types/pluginpb"
)

type generateService struct {
	logger          *zap.Logger
	client          v1alpha1.GenerateService
	contextModifier func(context.Context) context.Context
}

// GeneratePlugins generates an array of files given the provided
// module reference and plugin version and option tuples. No attempt
// is made at merging insertion points.
func (s *generateService) GeneratePlugins(
	ctx context.Context,
	image *v1.Image,
	plugins []*v1alpha1.PluginReference,
) (responses []*pluginpb.CodeGeneratorResponse, runtimeLibraries []*v1alpha1.RuntimeLibrary, _ error) {
	if s.contextModifier != nil {
		ctx = s.contextModifier(ctx)
	}
	response, err := s.client.GeneratePlugins(
		ctx,
		&v1alpha1.GeneratePluginsRequest{
			Image:   image,
			Plugins: plugins,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	return response.Responses, response.RuntimeLibraries, nil
}

// GenerateTemplate generates an array of files given the provided
// module reference and template version.
func (s *generateService) GenerateTemplate(
	ctx context.Context,
	image *v1.Image,
	templateOwner string,
	templateName string,
	templateVersion string,
) (files []*v1alpha1.File, runtimeLibraries []*v1alpha1.RuntimeLibrary, _ error) {
	if s.contextModifier != nil {
		ctx = s.contextModifier(ctx)
	}
	response, err := s.client.GenerateTemplate(
		ctx,
		&v1alpha1.GenerateTemplateRequest{
			Image:           image,
			TemplateOwner:   templateOwner,
			TemplateName:    templateName,
			TemplateVersion: templateVersion,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	return response.Files, response.RuntimeLibraries, nil
}
