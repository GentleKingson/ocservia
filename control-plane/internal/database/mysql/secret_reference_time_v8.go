package mysql

func SecretReferenceTimeSteps() []LongKeyStep {
	return TimeColumnSteps("secret_provider_refs", []TimeColumn{
		{Name: "rotated_at", Nullable: true}, {Name: "created_at"}, {Name: "updated_at"},
	}, "HEX(source.id)", nil, nil)
}
