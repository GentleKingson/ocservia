package contractpolicy_test

import (
	"strings"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestEnumZeroValuesAreUnspecified(t *testing.T) {
	t.Parallel()

	files := []protoreflect.FileDescriptor{
		agentv1.File_ocserv_platform_agent_v1_agent_proto,
		transportv1.File_ocserv_platform_transport_v1_transport_proto,
	}

	for _, file := range files {
		file := file
		t.Run(file.Path(), func(t *testing.T) {
			t.Parallel()
			assertEnumZeroValues(t, file.Enums())
			messages := file.Messages()
			for index := 0; index < messages.Len(); index++ {
				assertMessageEnums(t, messages.Get(index))
			}
		})
	}
}

func assertMessageEnums(t *testing.T, message protoreflect.MessageDescriptor) {
	t.Helper()
	assertEnumZeroValues(t, message.Enums())

	nested := message.Messages()
	for index := 0; index < nested.Len(); index++ {
		assertMessageEnums(t, nested.Get(index))
	}
}

func assertEnumZeroValues(t *testing.T, enums protoreflect.EnumDescriptors) {
	t.Helper()
	for index := 0; index < enums.Len(); index++ {
		enum := enums.Get(index)
		zero := enum.Values().ByNumber(0)
		if zero == nil || !strings.HasSuffix(string(zero.Name()), "UNSPECIFIED") {
			t.Errorf("enum %s must define an UNSPECIFIED zero value", enum.FullName())
		}
	}
}
