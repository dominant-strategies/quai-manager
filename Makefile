.PHONY publish:
	docker build -t quainetwork/quai-manager .
	docker push quainetwork/quai-manager